package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      any             `json:"id"`
}

type settings struct {
	MaxDepth int `json:"maxDepth"`
	MaxFiles int `json:"maxFiles"`
}

type nfoFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"`
}

type hostReply struct {
	result json.RawMessage
	err    string
}

var (
	mu       sync.Mutex
	nextHost = int64(1)
	pending  = map[int64]chan hostReply{}
	outMu    sync.Mutex
)

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for in.Scan() {
		line := append([]byte(nil), in.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if _, hasMethod := probe["method"]; !hasMethod {
			var resp struct {
				ID     any             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			idNum, ok := asInt64(resp.ID)
			if !ok {
				continue
			}
			mu.Lock()
			ch := pending[idNum]
			delete(pending, idNum)
			mu.Unlock()
			if ch == nil {
				continue
			}
			msg := ""
			if resp.Error != nil {
				msg = resp.Error.Message
			}
			ch <- hostReply{result: resp.Result, err: msg}
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		go handleRequest(req)
	}
}

func handleRequest(req request) {
	switch req.Method {
	case "fleet.handshake", "fleet.ping", "ext.activate", "ext.deactivate", "ext.health", "ext.upgrade":
		reply(req.ID, map[string]any{"ok": true})
	case "fleet.shutdown":
		reply(req.ID, map[string]any{"ok": true})
		os.Exit(0)
	case "ext.nfo.getSettings":
		st, err := loadSettings()
		if err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, publicSettings(st))
	case "ext.nfo.saveSettings":
		handleSave(req.ID, req.Params)
	case "ext.nfo.find":
		handleFind(req.ID, req.Params)
	case "ext.nfo.view":
		handleView(req.ID, req.Params)
	default:
		replyErr(req.ID, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func handleSave(id any, params json.RawMessage) {
	var p settings
	if err := json.Unmarshal(params, &p); err != nil {
		replyErr(id, "invalid settings")
		return
	}
	st := normalizeSettings(p)
	if err := saveSettings(&st); err != nil {
		replyErr(id, err.Error())
		return
	}
	reply(id, publicSettings(st))
}

func handleFind(id any, params json.RawMessage) {
	var p struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		replyErr(id, "invalid params")
		return
	}
	hash := strings.ToUpper(strings.TrimSpace(p.Hash))
	if hash == "" {
		replyErr(id, "hash required")
		return
	}
	st, err := loadSettings()
	if err != nil {
		replyErr(id, err.Error())
		return
	}
	dir, err := torrentDirectory(hash)
	if err != nil {
		replyErr(id, err.Error())
		return
	}
	files, err := findNFOs(dir, st.MaxDepth, st.MaxFiles)
	if err != nil {
		replyErr(id, err.Error())
		return
	}
	reply(id, map[string]any{"hash": hash, "directory": dir, "files": files})
}

func handleView(id any, params json.RawMessage) {
	var p struct {
		Hash string `json:"hash"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		replyErr(id, "invalid params")
		return
	}
	path := strings.TrimSpace(p.Path)
	hash := strings.ToUpper(strings.TrimSpace(p.Hash))
	if path == "" {
		if hash == "" {
			replyErr(id, "path or hash required")
			return
		}
		st, err := loadSettings()
		if err != nil {
			replyErr(id, err.Error())
			return
		}
		dir, err := torrentDirectory(hash)
		if err != nil {
			replyErr(id, err.Error())
			return
		}
		files, err := findNFOs(dir, st.MaxDepth, st.MaxFiles)
		if err != nil {
			replyErr(id, err.Error())
			return
		}
		if len(files) == 0 {
			replyErr(id, "no NFO found for torrent")
			return
		}
		path = files[0].Path
	}
	if !isNFOName(filepath.Base(path)) {
		replyErr(id, "path must be a .nfo file")
		return
	}
	raw, err := readFileBytes(path)
	if err != nil {
		replyErr(id, err.Error())
		return
	}
	text, enc := decodeNFOBytes(raw)
	_ = saveLastView(path, enc)
	reply(id, map[string]any{"path": path, "text": text, "encoding": enc, "bytes": len(raw)})
}

func torrentDirectory(hash string) (string, error) {
	raw, err := hostCall("fleet.torrents.list", map[string]any{})
	if err != nil {
		return "", err
	}
	var wrap struct {
		Torrents []struct {
			Hash      string `json:"hash"`
			Directory string `json:"directory"`
		} `json:"torrents"`
	}
	_ = json.Unmarshal(raw, &wrap)
	for _, t := range wrap.Torrents {
		if strings.EqualFold(t.Hash, hash) {
			dir := strings.TrimSpace(t.Directory)
			if dir == "" {
				return "", fmt.Errorf("torrent has no directory")
			}
			return dir, nil
		}
	}
	return "", fmt.Errorf("torrent not found")
}

func findNFOs(root string, maxDepth, maxFiles int) ([]nfoFile, error) {
	if maxDepth < 1 {
		maxDepth = 4
	}
	if maxFiles < 1 {
		maxFiles = 50
	}
	type node struct {
		path  string
		depth int
	}
	queue := []node{{path: root, depth: 0}}
	out := make([]nfoFile, 0)
	seen := 0
	for len(queue) > 0 && len(out) < maxFiles {
		n := queue[0]
		queue = queue[1:]
		if n.depth > maxDepth {
			continue
		}
		// If root itself is a .nfo file path, accept it.
		if n.depth == 0 && isNFOName(filepath.Base(n.path)) {
			out = append(out, nfoFile{Path: n.path, Name: filepath.Base(n.path)})
			continue
		}
		listRaw, err := hostCall("fleet.files.list", map[string]any{"path": n.path})
		if err != nil {
			if n.depth == 0 && isNFOName(filepath.Base(n.path)) {
				out = append(out, nfoFile{Path: n.path, Name: filepath.Base(n.path)})
			}
			continue
		}
		var listed struct {
			Entries []struct {
				Name  string `json:"name"`
				Path  string `json:"path"`
				IsDir bool   `json:"isDir"`
				Size  int64  `json:"size"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(listRaw, &listed)
		for _, e := range listed.Entries {
			seen++
			if seen > maxFiles*20 {
				return out, nil
			}
			p := strings.TrimSpace(e.Path)
			if p == "" {
				p = filepath.Join(n.path, e.Name)
			}
			if !e.IsDir && isNFOName(e.Name) {
				out = append(out, nfoFile{Path: p, Name: e.Name, Size: e.Size})
				if len(out) >= maxFiles {
					return out, nil
				}
				continue
			}
			if e.IsDir && n.depth < maxDepth {
				queue = append(queue, node{path: p, depth: n.depth + 1})
			}
		}
	}
	return out, nil
}

func readFileBytes(path string) ([]byte, error) {
	raw, err := hostCall("fleet.files.readBase64", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	var wrap struct {
		DataURL string `json:"dataUrl"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	dataURL := strings.TrimSpace(wrap.DataURL)
	idx := strings.Index(dataURL, "base64,")
	if idx < 0 {
		return nil, fmt.Errorf("invalid dataUrl")
	}
	return base64.StdEncoding.DecodeString(dataURL[idx+7:])
}

func publicSettings(st settings) map[string]any {
	return map[string]any{"maxDepth": st.MaxDepth, "maxFiles": st.MaxFiles}
}

func normalizeSettings(p settings) settings {
	if p.MaxDepth < 1 || p.MaxDepth > 8 {
		p.MaxDepth = 4
	}
	if p.MaxFiles < 1 || p.MaxFiles > 200 {
		p.MaxFiles = 50
	}
	return p
}

func loadSettings() (settings, error) {
	raw, err := hostCall("fleet.storage.get", map[string]any{"key": "settings"})
	if err != nil {
		return settings{}, err
	}
	st := settings{MaxDepth: 4, MaxFiles: 50}
	if string(raw) == "null" || len(raw) == 0 {
		return st, nil
	}
	_ = json.Unmarshal(raw, &st)
	return normalizeSettings(st), nil
}

func saveSettings(st *settings) error {
	_, err := hostCall("fleet.storage.put", map[string]any{"key": "settings", "value": publicSettings(*st)})
	return err
}

func saveLastView(path, encoding string) error {
	_, err := hostCall("fleet.storage.put", map[string]any{
		"key": "lastView",
		"value": map[string]any{
			"path": path, "encoding": encoding, "time": time.Now().UTC().Format(time.RFC3339),
		},
	})
	return err
}

func hostCall(method string, params any) (json.RawMessage, error) {
	ch := make(chan hostReply, 1)
	mu.Lock()
	id := nextHost
	nextHost++
	pending[id] = ch
	mu.Unlock()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	if err != nil {
		mu.Lock()
		delete(pending, id)
		mu.Unlock()
		return nil, err
	}
	outMu.Lock()
	fmt.Fprintf(os.Stdout, "%s\n", b)
	outMu.Unlock()
	select {
	case reply := <-ch:
		if reply.err != "" {
			return nil, fmt.Errorf("%s", reply.err)
		}
		if len(reply.result) == 0 {
			return json.RawMessage("null"), nil
		}
		return reply.result, nil
	case <-time.After(60 * time.Second):
		mu.Lock()
		delete(pending, id)
		mu.Unlock()
		return nil, fmt.Errorf("host call timeout")
	}
}

func reply(id any, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "result": result, "id": id})
	outMu.Lock()
	fmt.Fprintf(os.Stdout, "%s\n", b)
	outMu.Unlock()
}

func replyErr(id any, msg string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": -32000, "message": msg},
		"id":      id,
	})
	outMu.Lock()
	fmt.Fprintf(os.Stdout, "%s\n", b)
	outMu.Unlock()
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}
