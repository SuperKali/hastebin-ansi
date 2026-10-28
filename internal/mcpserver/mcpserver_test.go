package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/armbian/ansi-hastebin/handler"
	"github.com/armbian/ansi-hastebin/internal/keygenerator"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// setup starts the MCP endpoint on a test server and connects a real MCP client
func setup(t *testing.T, maxLength int) (*mcp.ClientSession, *handler.DocumentHandler, *httptest.Server) {
	t.Helper()

	docs := handler.NewDocumentHandler(10, maxLength, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	ts := httptest.NewServer(NewHandler(docs, Options{}))
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })

	return session, docs, ts
}

func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, *mcp.CallToolResult) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text, res
}

func upload(t *testing.T, session *mcp.ClientSession, content string) string {
	t.Helper()
	_, res := call(t, session, "upload_log", map[string]any{"content": content})
	require.False(t, res.IsError)
	out, ok := res.StructuredContent.(map[string]any)
	require.True(t, ok)
	return out["key"].(string)
}

func numberedLog(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestInitializeAndListTools(t *testing.T) {
	session, _, _ := setup(t, 1024)

	init := session.InitializeResult()
	require.Equal(t, "hastebin", init.ServerInfo.Name)
	require.Contains(t, init.Instructions, "public")
	require.Contains(t, init.Instructions, "curl --data-binary @file")
	require.Contains(t, init.Instructions, "untrusted data")

	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	byName := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		byName[tool.Name] = tool
	}
	require.Len(t, byName, 3)

	up := byName["upload_log"]
	require.Contains(t, up.Description, "remove secrets")
	require.False(t, up.Annotations.ReadOnlyHint)
	require.False(t, *up.Annotations.DestructiveHint)
	require.True(t, *up.Annotations.OpenWorldHint)

	for _, name := range []string{"read_paste", "search_paste"} {
		require.True(t, byName[name].Annotations.ReadOnlyHint, name)
		require.Contains(t, byName[name].Description, "untrusted data", name)
	}
}

func TestUploadThenRead(t *testing.T) {
	session, docs, ts := setup(t, 1<<20)

	text, res := call(t, session, "upload_log", map[string]any{"content": "hello\nworld\n", "extension": "log"})
	require.False(t, res.IsError, text)

	out := res.StructuredContent.(map[string]any)
	key := out["key"].(string)
	host := strings.TrimPrefix(ts.URL, "http://")
	require.Equal(t, "https://"+host+"/"+key+".log", out["url"])
	require.Equal(t, "https://"+host+"/raw/"+key, out["raw_url"])
	require.EqualValues(t, 12, out["size"])
	require.EqualValues(t, 2, out["lines"])
	require.Contains(t, text, out["url"])

	stored, err := docs.Store.Get(key, false)
	require.NoError(t, err)
	require.Equal(t, "hello\nworld\n", stored)

	text, res = call(t, session, "read_paste", map[string]any{"key_or_url": out["url"]})
	require.False(t, res.IsError, text)
	require.Contains(t, text, "total_lines: 2\n")
	require.Contains(t, text, "total_bytes: 12\n")
	require.Contains(t, text, "next_offset: none")
	require.Contains(t, text, "     1  hello\n")
	require.Contains(t, text, "     2  world\n")
}

func TestUploadBaseURL(t *testing.T) {
	session, docs, _ := setup(t, 1024)
	docs.BaseURL = "https://paste.example.com"

	_, res := call(t, session, "upload_log", map[string]any{"content": "x"})
	out := res.StructuredContent.(map[string]any)
	require.Equal(t, "https://paste.example.com/"+out["key"].(string), out["url"])
}

func TestUploadErrors(t *testing.T) {
	session, _, _ := setup(t, 16)

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "oversize", args: map[string]any{"content": strings.Repeat("a", 17)}, want: "upload_log accepts up to 16 bytes"},
		{name: "empty", args: map[string]any{"content": "  \n"}, want: "content is empty"},
		{name: "missing", args: map[string]any{}, want: "content is empty"},
		{name: "bad extension", args: map[string]any{"content": "x", "extension": "../x"}, want: "invalid extension"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, res := call(t, session, "upload_log", tt.args)
			require.True(t, res.IsError)
			require.Contains(t, text, tt.want)
		})
	}

	// The body limit is twice the upload limit plus the JSON framing allowance
	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "upload_log",
		Arguments: map[string]any{"content": strings.Repeat("a", 2*16+JSONOverhead)},
	})
	require.Error(t, err)
}

func TestUploadMCPMaxLength(t *testing.T) {
	// The MCP limit is lower than the paste limit
	docs := handler.NewDocumentHandler(10, 1<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	docs.BaseURL = "https://paste.example.com"
	ts := httptest.NewServer(NewHandler(docs, Options{MaxLength: 100}))
	t.Cleanup(ts.Close)
	session := connect(t, ts.URL)

	init := session.InitializeResult()
	require.Contains(t, init.Instructions, "up to 100 bytes")
	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name == "upload_log" {
			require.Contains(t, tool.Description, "up to 100 bytes")
		}
	}

	upload(t, session, strings.Repeat("a", 100))

	// Over it, the agent is told to upload with curl
	text, res := call(t, session, "upload_log", map[string]any{"content": strings.Repeat("a", 101)})
	require.True(t, res.IsError)
	require.Contains(t, text, "content is 101 bytes")
	require.Contains(t, text, "curl --data-binary @file https://paste.example.com/log")

	// A body over twice the limit plus the framing room is refused before it is read
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"` +
		strings.Repeat("a", 200+JSONOverhead) + `"}}}`
	resp := postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), `"error"`)
	require.Contains(t, string(data), "curl --data-binary @file https://paste.example.com/log")

	// The same body of unknown length is cut while it is read
	resp = postMCP(t, ts.URL, strings.NewReader(body), -1)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)

	// Just under the cap it is read and parsed
	content := strings.Repeat("a", 101)
	body = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"` + content + `"}}}`
	resp = postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestMaxLength(t *testing.T) {
	require.Equal(t, DefaultMaxLength, MaxLength(0, 0))
	require.Equal(t, DefaultMaxLength, MaxLength(-1, 64<<20))
	require.Equal(t, 4000000, MaxLength(0, 4000000))
	require.Equal(t, 1<<20, MaxLength(1<<20, 64<<20))
	require.Equal(t, 1024, MaxLength(1<<20, 1024))
}

func TestUploadBusy(t *testing.T) {
	session, docs, ts := setup(t, 4*handler.LargeUpload)
	docs.LimitUploads(1)

	release, ok := docs.AcquireUpload("", -1)
	require.True(t, ok)

	// A large request is refused before its body is read
	large := strings.Repeat("a", handler.LargeUpload+1)
	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "upload_log", Arguments: map[string]any{"content": large}})
	require.Error(t, err)

	// Small uploads do not need a slot
	upload(t, connect(t, ts.URL), "small")

	// With the slot free, the request takes it once: the tool must not ask for a second one
	release()
	text, res := call(t, connect(t, ts.URL), "upload_log", map[string]any{"content": large})
	require.False(t, res.IsError, text)
}

// connect opens a new client session on the test server
func connect(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })
	return session
}

// postMCP sends one raw JSON-RPC request to the endpoint
func postMCP(t *testing.T, url string, body io.Reader, size int64) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	require.NoError(t, err)
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestLargeRequestHoldsSlot(t *testing.T) {
	_, docs, ts := setup(t, 4*handler.LargeUpload)
	docs.LimitUploads(1)

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"`

	// A large request of unknown size that is still arriving holds the only slot
	pr, pw := io.Pipe()
	first := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, pr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- nil
			return
		}
		first <- resp
	}()
	_, err := pw.Write([]byte(call))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return docs.UploadsInProgress() == 1 }, 2*time.Second, 10*time.Millisecond)

	// A second large request gets 503 with a JSON-RPC error
	body := call + strings.Repeat("b", handler.LargeUpload+1) + `"}}}`
	resp := postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), `"error"`)
	require.Contains(t, string(data), "busy")

	// Small requests still pass
	small := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	resp = postMCP(t, ts.URL, strings.NewReader(small), int64(len(small)))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The first request finishes and frees the slot
	_, err = pw.Write([]byte(strings.Repeat("a", handler.LargeUpload+1) + `"}}}`))
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	resp = <-first
	require.NotNil(t, resp)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	data, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), "Uploaded")
	require.NotContains(t, string(data), "busy")

	require.Equal(t, 0, docs.UploadsInProgress())
}

func TestLargeRequestPerIP(t *testing.T) {
	_, docs, ts := setup(t, 4*handler.LargeUpload)
	docs.LimitUploads(4)
	docs.LimitUploadsPerIP(1)

	// The test client connects from 127.0.0.1, which already holds its one slot
	release, ok := docs.AcquireUpload("127.0.0.1", -1)
	require.True(t, ok)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"` +
		strings.Repeat("a", handler.LargeUpload+1) + `"}}}`
	resp := postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	release()
	resp = postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSlowRequestDropped(t *testing.T) {
	_, docs, ts := setup(t, 4*handler.LargeUpload)
	docs.MinUploadBytes = 1000
	docs.UploadWindow = 200 * time.Millisecond

	// A few bytes now and then, far below the minimum rate
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"`))
		for i := 0; i < 20; i++ {
			if _, err := pw.Write([]byte("aaaa")); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		pw.Close()
	}()

	resp := postMCP(t, ts.URL, pr, -1)
	require.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
	pw.CloseWithError(io.ErrClosedPipe)

	// The slot is free again
	release, ok := docs.AcquireUpload("", -1)
	require.True(t, ok)
	release()
}

func TestReadPaging(t *testing.T) {
	session, _, _ := setup(t, 1<<20)
	key := upload(t, session, numberedLog(1000))

	// Default page
	text, _ := call(t, session, "read_paste", map[string]any{"key_or_url": key})
	require.Contains(t, text, "lines: 1-400\n")
	require.Contains(t, text, "total_lines: 1000\n")
	require.Contains(t, text, "next_offset: 401\n")
	require.Contains(t, text, "   400  line 400\n")
	require.NotContains(t, text, "line 401\n")

	// Next pages
	text, _ = call(t, session, "read_paste", map[string]any{"key_or_url": key, "offset_line": 401, "max_lines": 10})
	require.Contains(t, text, "lines: 401-410\n")
	require.Contains(t, text, "next_offset: 411\n")
	require.Contains(t, text, "   401  line 401\n")

	text, _ = call(t, session, "read_paste", map[string]any{"key_or_url": key, "offset_line": 995, "max_lines": 100})
	require.Contains(t, text, "lines: 995-1000\n")
	require.Contains(t, text, "next_offset: none")

	// max_lines is capped
	key = upload(t, session, numberedLog(3000))
	text, _ = call(t, session, "read_paste", map[string]any{"key_or_url": key, "max_lines": 5000})
	require.Contains(t, text, "lines: 1-2000\n")
	require.Contains(t, text, "next_offset: 2001\n")

	// Past the end
	text, res := call(t, session, "read_paste", map[string]any{"key_or_url": key, "offset_line": 9999})
	require.False(t, res.IsError)
	require.Contains(t, text, "past the end")
	require.Contains(t, text, "next_offset: none")
}

func TestReadByteCap(t *testing.T) {
	session, _, _ := setup(t, 4<<20)

	// 100 lines of 10 KiB: the 256 KiB budget ends the page early
	line := strings.Repeat("x", 10<<10)
	key := upload(t, session, strings.Repeat(line+"\n", 100))
	text, _ := call(t, session, "read_paste", map[string]any{"key_or_url": key, "max_lines": 100})
	require.LessOrEqual(t, len(text), maxOutputBytes+1024)
	require.Contains(t, text, "lines: 1-25\n")
	require.Contains(t, text, "next_offset: 26\n")
	require.Contains(t, text, "output limited")

	// One line longer than the budget is cut
	key = upload(t, session, strings.Repeat("y", 1<<20)+"\nshort\n")
	text, _ = call(t, session, "read_paste", map[string]any{"key_or_url": key})
	require.LessOrEqual(t, len(text), maxOutputBytes+1024)
	require.Contains(t, text, "line cut")
	require.Contains(t, text, "next_offset: 2\n")
	require.Contains(t, text, "/raw/"+key)

	// A cut last line points to the raw paste, not to a next page that does not exist
	key = upload(t, session, strings.Repeat("z", 1<<20)+"\n")
	text, _ = call(t, session, "read_paste", map[string]any{"key_or_url": key})
	require.LessOrEqual(t, len(text), maxOutputBytes+1024)
	require.Contains(t, text, "next_offset: none")
	require.Contains(t, text, "line 1 is longer than the 256 KiB output limit")
	require.Contains(t, text, "/raw/"+key+"\n")
	require.NotContains(t, text, "read on from next_offset")
}

func TestReadStripsANSI(t *testing.T) {
	session, _, _ := setup(t, 1024)
	key := upload(t, session, "\x1b[31mred\x1b[0m text\r\n")

	text, _ := call(t, session, "read_paste", map[string]any{"key_or_url": key})
	require.Contains(t, text, "     1  red text\n")

	text, _ = call(t, session, "read_paste", map[string]any{"key_or_url": key, "keep_ansi": true})
	require.Contains(t, text, "\x1b[31mred")
}

func TestSearch(t *testing.T) {
	session, _, _ := setup(t, 1<<20)
	content := "boot ok\nstep 1\nstep 2\nERROR disk full\nstep 4\nstep 5\nstep 6\nstep 7\nstep 8\nerror again\nend\n"
	key := upload(t, session, content)

	text, res := call(t, session, "search_paste", map[string]any{"key_or_url": key, "pattern": "error", "ignore_case": true})
	require.False(t, res.IsError, text)
	require.Contains(t, text, "matches: 2\n")
	require.Contains(t, text, "2-step 1\n3-step 2\n4:ERROR disk full\n5-step 4\n6-step 5\n--\n8-step 7\n9-step 8\n10:error again\n11-end\n")

	// Case sensitive by default
	text, _ = call(t, session, "search_paste", map[string]any{"key_or_url": key, "pattern": "ERROR", "context_lines": 0})
	require.Contains(t, text, "matches: 1\n")
	require.Contains(t, text, "---\n4:ERROR disk full\n")

	// max_matches limits what is shown, not the count
	text, _ = call(t, session, "search_paste", map[string]any{"key_or_url": key, "pattern": "^step", "context_lines": 0, "max_matches": 2})
	require.Contains(t, text, "matches: 7 (showing the first 2)\n")
	require.Contains(t, text, "2:step 1\n3:step 2\n")
	require.NotContains(t, text, "step 4")

	// Overlapping context is merged, context is capped at 10
	text, _ = call(t, session, "search_paste", map[string]any{"key_or_url": key, "pattern": "step [12]$", "context_lines": 50})
	require.Equal(t, 1, strings.Count(text, "1-boot ok\n"))
	require.NotContains(t, text, "\n--\n")

	text, _ = call(t, session, "search_paste", map[string]any{"key_or_url": key, "pattern": "nothing here"})
	require.Contains(t, text, "matches: 0\n")
	require.Contains(t, text, "no matching lines")
}

// searchService builds the tool service on a store holding one paste, for calling search directly
func searchService(t *testing.T, content string) (*service, string) {
	t.Helper()
	docs := handler.NewDocumentHandler(10, 1<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	key, err := docs.CreateDocument(content)
	require.NoError(t, err)
	return newService(docs, 0), key
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

func TestSearchCancelled(t *testing.T) {
	svc, key := searchService(t, numberedLog(10000))

	// A client that went away stops the scan at the next check
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _, err := svc.search(ctx, nil, SearchInput{KeyOrURL: key, Pattern: "line"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, res)

	// The search slot is given back
	require.Zero(t, svc.searches.inUse())

	// The SDK runs tools on a context detached from the HTTP request: a request
	// that ended stops the scan too
	req, cancelReq := context.WithCancel(context.Background())
	cancelReq()
	ctx = context.WithValue(context.Background(), requestKey{}, req)
	_, _, err = svc.search(ctx, nil, SearchInput{KeyOrURL: key, Pattern: "line"})
	require.ErrorIs(t, err, context.Canceled)
}

// clientServer serves the MCP endpoint with the client address taken from the
// X-Test-Client header, so one test can act as several clients
func clientServer(t *testing.T, docs *handler.DocumentHandler) *httptest.Server {
	t.Helper()
	mcpHandler := NewHandler(docs, Options{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := r.Header.Get("X-Test-Client"); ip != "" {
			r.RemoteAddr = ip + ":1"
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// callAs sends one tools/call from the client at ip and returns the HTTP error or the result text
func callAs(ctx context.Context, url, ip, tool string, args map[string]any) (string, error) {
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + string(params) + `}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Test-Client", ip)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

func TestSearchStopsWhenClientLeaves(t *testing.T) {
	// Two slow searches from two clients take both slots, then their clients
	// hang up long before the budget ends. About 5 seconds of scanning without
	// race detection, more with it.
	docs := handler.NewDocumentHandler(10, 16<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	ts := clientServer(t, docs)
	key, err := docs.CreateDocument(strings.Repeat("word word word word word word word word word word word word\n", 12<<20/60))
	require.NoError(t, err)
	small, err := docs.CreateDocument("x\n")
	require.NoError(t, err)

	start := time.Now()
	done := make(chan error, maxToolSearches)
	for i := range maxToolSearches {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := callAs(ctx, ts.URL, fmt.Sprintf("192.0.2.%d", i+1), "search_paste", map[string]any{"key_or_url": key, "pattern": `(?:\w+\s*){1,300}Z`})
			done <- err
		}()
	}
	for range maxToolSearches {
		require.Error(t, <-done)
	}

	// Both slots free up well before the 5 second budget
	require.Eventually(t, func() bool {
		text, err := callAs(context.Background(), ts.URL, "192.0.2.1", "search_paste", map[string]any{"key_or_url": small, "pattern": "nothing"})
		return err == nil && strings.Contains(text, "matches: 0")
	}, 3*time.Second, 50*time.Millisecond)
	require.Less(t, time.Since(start), 4*time.Second)
}

func TestSearchesDoNotBlockReads(t *testing.T) {
	// One client keeps a slow search running: its second search is refused,
	// while another client reads, and searches too
	docs := handler.NewDocumentHandler(10, 16<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	ts := clientServer(t, docs)
	key, err := docs.CreateDocument(strings.Repeat("word word word word word word word word word word word word\n", 12<<20/60))
	require.NoError(t, err)
	small, err := docs.CreateDocument("x\n")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go callAs(ctx, ts.URL, "2001:db8::1", "search_paste", map[string]any{"key_or_url": key, "pattern": `(?:\w+\s*){1,300}Z`})

	// Wait until the slow search runs
	require.Eventually(t, func() bool {
		text, _ := callAs(context.Background(), ts.URL, "2001:db8::2", "search_paste", map[string]any{"key_or_url": small, "pattern": "x"})
		return strings.Contains(text, "another search_paste call from your address is still running")
	}, 3*time.Second, 10*time.Millisecond)

	text, err := callAs(context.Background(), ts.URL, "192.0.2.7", "read_paste", map[string]any{"key_or_url": small})
	require.NoError(t, err)
	require.Contains(t, text, "total_lines: 1")
	text, err = callAs(context.Background(), ts.URL, "2001:db8::3", "read_paste", map[string]any{"key_or_url": small})
	require.NoError(t, err)
	require.Contains(t, text, "total_lines: 1", "the client running a search can still read")
	text, err = callAs(context.Background(), ts.URL, "192.0.2.7", "search_paste", map[string]any{"key_or_url": small, "pattern": "x"})
	require.NoError(t, err)
	require.Contains(t, text, "matches: 1")
}

func TestSearchTimeBudget(t *testing.T) {
	svc, key := searchService(t, numberedLog(10000))
	svc.searchBudget = time.Nanosecond

	// The budget is checked every 4096 lines: the matches so far are returned
	res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: "^line", ContextLines: 1, MaxMatches: 200})
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := resultText(t, res)
	require.Contains(t, text, "matches: 4096 (showing the first 200)\n")
	require.Contains(t, text, "truncated: time budget reached after 1ns, only lines 1-4096 of 10000 were searched")
	require.Contains(t, text, "1:line 1\n")

	// With the normal budget everything is searched
	svc.searchBudget = defaultSearchBudget
	res, _, err = svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: "^line", MaxMatches: 1})
	require.NoError(t, err)
	text = resultText(t, res)
	require.Contains(t, text, "matches: 10000 ")
	require.NotContains(t, text, "truncated")
}

func TestSearchBusy(t *testing.T) {
	svc, key := searchService(t, "x\n")
	as := func(ip string) context.Context { return context.WithValue(context.Background(), clientIPKey{}, ip) }

	// Every search slot is taken: searches are refused, reads are not
	var held []func()
	for i := range maxToolSearches {
		release, res := svc.searches.acquire(fmt.Sprintf("192.0.2.%d", i+1))
		require.Nil(t, res)
		held = append(held, release)
	}
	res, _, err := svc.search(as("198.51.100.1"), nil, SearchInput{KeyOrURL: key, Pattern: "x"})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "server busy, retry")
	res, _, err = svc.read(as("198.51.100.1"), nil, ReadInput{KeyOrURL: key})
	require.NoError(t, err)
	require.False(t, res.IsError)

	// Every read slot is taken
	for i := range maxToolReads {
		release, res := svc.reads.acquire(fmt.Sprintf("203.0.113.%d", i+1))
		require.Nil(t, res)
		held = append(held, release)
	}
	res, _, err = svc.read(as("198.51.100.1"), nil, ReadInput{KeyOrURL: key})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "server busy, retry")

	for _, release := range held {
		release()
		release()
	}
	require.Zero(t, svc.reads.inUse())
	require.Zero(t, svc.searches.inUse())

	// One call of each tool per client, counting an IPv6 /64 as one client
	release, res := svc.reads.acquire("2001:db8::1")
	require.Nil(t, res)
	res, _, err = svc.read(as("2001:db8::2"), nil, ReadInput{KeyOrURL: key})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "another read_paste call from your address is still running")
	res, _, err = svc.search(as("2001:db8::2"), nil, SearchInput{KeyOrURL: key, Pattern: "x"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	res, _, err = svc.read(as("2001:db8:0:1::1"), nil, ReadInput{KeyOrURL: key})
	require.NoError(t, err)
	require.False(t, res.IsError)
	release()
	require.Zero(t, svc.reads.inUse())
}

func TestSearchErrors(t *testing.T) {
	session, _, _ := setup(t, 1024)
	key := upload(t, session, "x\n")

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "bad regex", args: map[string]any{"key_or_url": key, "pattern": "(unclosed"}, want: "invalid regular expression"},
		{name: "long regex", args: map[string]any{"key_or_url": key, "pattern": strings.Repeat("a", 257)}, want: "longer than 256"},
		{name: "empty regex", args: map[string]any{"key_or_url": key, "pattern": ""}, want: "pattern is empty"},
		{name: "complex regex", args: map[string]any{"key_or_url": key, "pattern": `(?:\w+\s*){1,1000}Z`}, want: "too complex"},
		{name: "missing paste", args: map[string]any{"key_or_url": "nosuchkey", "pattern": "x"}, want: "not found"},
		{name: "bad key", args: map[string]any{"key_or_url": "../../etc/passwd", "pattern": "x"}, want: "invalid paste"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, res := call(t, session, "search_paste", tt.args)
			require.True(t, res.IsError)
			require.Contains(t, text, tt.want)
		})
	}
}

func TestReadBanned(t *testing.T) {
	session, docs, _ := setup(t, 1024)
	docs.NotFound = handler.NewNotFoundLimiter(2, time.Minute, time.Hour)
	key := upload(t, session, "hello\n")

	// The test client is 127.0.0.1: two unknown keys are allowed, the third bans it
	for _, name := range []string{"read_paste", "search_paste"} {
		args := map[string]any{"key_or_url": "nosuchkey" + name[:4]}
		if name == "search_paste" {
			args["pattern"] = "x"
		}
		text, res := call(t, session, name, args)
		require.True(t, res.IsError)
		require.Contains(t, text, "not found")
	}
	text, res := call(t, session, "read_paste", map[string]any{"key_or_url": "nosuchkey3"})
	require.True(t, res.IsError)
	require.Contains(t, text, "too many lookups of unknown keys")
	require.Contains(t, text, "retry in 3600 seconds")

	// Known pastes are blocked too, for both tools; uploads still work
	text, res = call(t, session, "read_paste", map[string]any{"key_or_url": key})
	require.True(t, res.IsError)
	require.Contains(t, text, "too many lookups")
	text, res = call(t, session, "search_paste", map[string]any{"key_or_url": key, "pattern": "hello"})
	require.True(t, res.IsError)
	require.Contains(t, text, "too many lookups")
	upload(t, session, "still allowed")
}

func TestReadErrors(t *testing.T) {
	session, _, _ := setup(t, 1024)

	for _, keyOrURL := range []string{"", "a b", "https://host/a/b/c", "https://host/", "ke$y"} {
		text, res := call(t, session, "read_paste", map[string]any{"key_or_url": keyOrURL})
		require.True(t, res.IsError, keyOrURL)
		require.Regexp(t, "key_or_url is empty|invalid paste", text, keyOrURL)
	}

	text, res := call(t, session, "read_paste", map[string]any{"key_or_url": "https://paste.example.com/nosuchkey.log"})
	require.True(t, res.IsError)
	require.Contains(t, text, `paste "nosuchkey" not found`)
}

func TestParseKey(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "abc123", want: "abc123"},
		{in: "  abc123\n", want: "abc123"},
		{in: "abc.pl", want: "abc"},
		{in: "https://paste.example.com/abc.pl", want: "abc"},
		{in: "https://paste.example.com/abc", want: "abc"},
		{in: "https://paste.example.com/raw/abc", want: "abc"},
		{in: "https://paste.example.com/raw/abc.txt", want: "abc"},
		{in: "https://paste.example.com/documents/abc", want: "abc"},
		{in: "http://localhost:7777/abc?x=1#l5", want: "abc"},
		{in: "paste.example.com/abc.log", want: "abc"},
		{in: "/raw/abc", want: "abc"},
		{in: "", wantErr: true},
		{in: "https://paste.example.com/", wantErr: true},
		{in: "https://paste.example.com/x/abc", wantErr: true},
		{in: "https://paste.example.com/raw/abc/more", wantErr: true},
		{in: "../etc/passwd", wantErr: true},
		{in: "..", wantErr: true},
		{in: "a b", wantErr: true},
		{in: strings.Repeat("a", 65), wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseKey(tt.in)
		if tt.wantErr {
			require.Error(t, err, tt.in)
			continue
		}
		require.NoError(t, err, tt.in)
		require.Equal(t, tt.want, got, tt.in)
	}
}

func TestHTTPEndpoint(t *testing.T) {
	_, _, ts := setup(t, 1024)

	// A browser gets a hint instead of a bare 405
	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	require.Equal(t, "POST", resp.Header.Get("Allow"))

	// The request body is limited before it is parsed
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"` +
		strings.Repeat("a", 2*1024+JSONOverhead) + `"}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestLineReader(t *testing.T) {
	for _, data := range []string{"\n", "a", "a\n", "a\nb", "a\nb\n", "\n\n", "a\n\nb\n\n", "a\r\nb\r\n"} {
		want := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
		var got []string
		lines := newLineReader(strings.NewReader(data))
		for {
			line, size, ok := lines.line(nil, 100)
			if !ok {
				break
			}
			require.Equal(t, len(line), size)
			got = append(got, string(line))
		}
		require.Equal(t, want, got, "%q", data)
		require.Equal(t, len(want), lines.count, "%q", data)
		require.Equal(t, len(want), countLines(data), "%q", data)
	}

	// Lines of any length, across the read buffer, keep at most limit bytes
	rng := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	var want []string
	for i := 0; i < 300; i++ {
		n := rng.IntN(3 * readBuffer)
		if i%7 == 0 {
			n = readBuffer + rng.IntN(3) - 1
		}
		line := strings.Repeat(string(rune('a'+i%26)), n)
		want = append(want, line)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	data := b.String()
	for _, limit := range []int{0, 1, 1000, readBuffer, maxCleanBytes} {
		lines := newLineReader(strings.NewReader(data))
		var buf []byte
		for i, line := range want {
			start, size, ok := lines.line(buf[:0], limit)
			require.True(t, ok)
			require.Equal(t, len(line), size, "line %d", i)
			require.Equal(t, line[:min(limit, len(line))], string(start), "line %d", i)
			buf = start
		}
		_, _, ok := lines.line(nil, limit)
		require.False(t, ok)
		require.Equal(t, len(want), lines.count)
		require.NoError(t, lines.err)
	}

	// Skipping and counting
	lines := newLineReader(strings.NewReader("a\nb\nc\n"))
	lines.line(nil, 0)
	lines.line(nil, 0)
	line, _, ok := lines.line(nil, 10)
	require.True(t, ok)
	require.Equal(t, "c", string(line))
	lines.drain()
	require.Equal(t, 3, lines.count)

	// A read error ends the paste and is kept
	lines = newLineReader(io.MultiReader(strings.NewReader("a\nb"), iotest.ErrReader(errors.New("disk gone"))))
	lines.drain()
	require.Equal(t, 2, lines.count)
	require.ErrorContains(t, lines.err, "disk gone")
}

// pieceMatch is how a line is matched when the whole paste is in memory: a
// line over searchChunkBytes in overlapping pieces
func pieceMatch(g *grep, raw string) bool {
	if len(raw) <= searchChunkBytes {
		return g.re.MatchString(cleanLine(raw, g.keepANSI))
	}
	for start := 0; ; {
		end := min(start+searchChunkBytes, len(raw))
		re := g.middle
		switch {
		case start == 0:
			re = g.first
		case end == len(raw):
			re = g.last
		}
		if re.MatchString(cleanLine(raw[start:end], g.keepANSI)) {
			return true
		}
		if end == len(raw) {
			return false
		}
		start = end - searchOverlap
	}
}

func TestSearchPiecesStreamed(t *testing.T) {
	// The streamed search finds what a search of the line held whole finds, piece for piece
	rng := rand.New(rand.NewPCG(3, 4))
	step := searchChunkBytes - searchOverlap
	sizes := []int{1, 100, searchChunkBytes - 1, searchChunkBytes, searchChunkBytes + 1, 2*step + searchOverlap,
		2*step + searchOverlap + 1, 3*searchChunkBytes + 17}
	var lines []string
	for _, size := range sizes {
		for range 6 {
			line := []byte(strings.Repeat("x", size))
			for _, marker := range []string{"START", "MID", "END"} {
				if size > 10 && rng.IntN(2) == 0 {
					at := rng.IntN(size - len(marker))
					switch marker {
					case "START":
						at = 0
					case "END":
						at = size - len(marker)
					}
					copy(line[at:], marker)
				}
			}
			lines = append(lines, string(line))
		}
	}
	data := strings.Join(lines, "\n") + "\n"

	for _, pattern := range []string{"MID", "^START", "END$", "^x", "x$", "^START|END$", `\AMID`, "TX{3}"} {
		re := regexp.MustCompile(pattern)
		g := &grep{ctx: context.Background(), re: re, deadline: time.Now().Add(time.Minute), piece: make([]byte, 0, searchChunkBytes)}
		g.first, g.middle, g.last = pieceRegexps(pattern, re)
		reader := newLineReader(strings.NewReader(data))
		for i, line := range lines {
			matched, _, size, ok, err := g.match(reader)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, len(line), size, "line %d", i)
			require.Equal(t, pieceMatch(g, line), matched, "pattern %q line %d of %d bytes", pattern, i, len(line))
		}
		_, _, _, ok, err := g.match(reader)
		require.NoError(t, err)
		require.False(t, ok)
		require.Equal(t, len(lines), reader.count)
	}
}

// loadedStore hides the streaming of file storage, as a backend that loads whole pastes
type loadedStore struct{ storage.Storage }

func TestToolsLoadedBackend(t *testing.T) {
	// Pastes loaded whole read and search the same as streamed ones
	contents := []string{
		numberedLog(3000),
		"no final newline\nsecond",
		"\x1b[31mred\x1b[0m\r\n\n\nERROR here\r\n",
		strings.Repeat("y", 3*searchChunkBytes) + "NEEDLE\nshort ERROR\n" + strings.Repeat("z", 1<<20) + "\n",
	}
	streamed := handler.NewDocumentHandler(10, 4<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	loaded := handler.NewDocumentHandler(10, 4<<20, loadedStore{storage.NewFileStorage(t.TempDir(), 0)}, keygenerator.NewRandomKeyGenerator(""))
	require.True(t, handler.Streams(streamed.Store))
	require.False(t, handler.Streams(loaded.Store))
	for _, content := range contents {
		var texts []string
		for _, docs := range []*handler.DocumentHandler{streamed, loaded} {
			key, err := docs.CreateDocument(content)
			require.NoError(t, err)
			svc := newService(docs, 0)
			var out strings.Builder
			for _, in := range []ReadInput{{}, {OffsetLine: 2, MaxLines: 3}, {OffsetLine: 2999}, {KeepANSI: true}, {OffsetLine: 99999}} {
				in.KeyOrURL = key
				res, _, err := svc.read(context.Background(), nil, in)
				require.NoError(t, err)
				out.WriteString(resultText(t, res))
			}
			for _, pattern := range []string{"ERROR|NEEDLE", "^line 2", "red", "^$"} {
				res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: pattern, ContextLines: 1})
				require.NoError(t, err)
				out.WriteString(resultText(t, res))
			}
			texts = append(texts, strings.ReplaceAll(out.String(), key, "KEY"))
			require.Zero(t, docs.ReadMemory.InUse())
		}
		require.Equal(t, texts[0], texts[1])
	}
}

func TestReadManyLines(t *testing.T) {
	// A paste of only newlines is read page by page without a slice of every line
	svc, key := searchService(t, strings.Repeat("\n", 100000))
	res, _, err := svc.read(context.Background(), nil, ReadInput{KeyOrURL: key, OffsetLine: 99999, MaxLines: 10})
	require.NoError(t, err)
	text := resultText(t, res)
	require.Contains(t, text, "lines: 99999-100000\n")
	require.Contains(t, text, "total_lines: 100000\n")
	require.Contains(t, text, " 99999  \n100000  \n")
	require.Contains(t, text, "next_offset: none")
}

func TestReadLongLineANSI(t *testing.T) {
	// Only the shown start of a huge line is cleaned of ANSI codes
	line := strings.Repeat("\x1b[31mred\x1b[0m ", 60000)
	svc, key := searchService(t, line+"\nnext\n")
	res, _, err := svc.read(context.Background(), nil, ReadInput{KeyOrURL: key})
	require.NoError(t, err)
	text := resultText(t, res)
	require.LessOrEqual(t, len(text), maxOutputBytes+1024)
	require.NotContains(t, text, "\x1b")
	require.Contains(t, text, "     1  red red red")
	require.Contains(t, text, fmt.Sprintf(" of %d bytes shown]\n", len(line)))
	require.Contains(t, text, "line 1 is longer than the 256 KiB output limit")
	require.Contains(t, text, "     2  next\n")
}

func TestSearchLongLine(t *testing.T) {
	// A match deep inside a line longer than one piece is found, also across
	// the edge of two pieces, and the line is printed cut
	long := strings.Repeat("a", 3*searchChunkBytes) + "NEEDLE" + strings.Repeat("b", searchChunkBytes)
	edge := strings.Repeat("c", searchChunkBytes-3) + "EDGE" + strings.Repeat("d", searchChunkBytes)
	svc, key := searchService(t, "first\n"+long+"\n"+edge+"\nlast\n")

	res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: "NEEDLE|EDGE", ContextLines: 1})
	require.NoError(t, err)
	text := resultText(t, res)
	require.Contains(t, text, "matches: 2\n")
	require.Contains(t, text, "1-first\n")
	require.Contains(t, text, fmt.Sprintf("2:%s [+%d bytes]\n", strings.Repeat("a", maxSearchLineBytes), len(long)-maxSearchLineBytes))
	require.Contains(t, text, "3:ccc")
	require.Contains(t, text, "4-last\n")

	res, _, err = svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: "missing"})
	require.NoError(t, err)
	require.Contains(t, resultText(t, res), "matches: 0\n")
	require.Contains(t, resultText(t, res), "approximate: true\n")
}

func TestSearchLongLineAnchors(t *testing.T) {
	// In a line searched in pieces, ^ and $ hold only at the true start and end of the line
	long := "START" + strings.Repeat("a", 2*searchChunkBytes) + "MID" + strings.Repeat("b", 2*searchChunkBytes) + "END"
	svc, key := searchService(t, long+"\nshort\n")

	tests := []struct {
		pattern string
		matches int
	}{
		{"^START", 1},
		{"^a", 0},
		{"^b", 0},
		{"END$", 1},
		{"a$", 0},
		{"b$", 0},
		{"^MID|END$", 1},
		{"^(?:a|short)", 1},
		{`\Ab|^short$`, 1},
		{"(?i)^start", 1},
		{"(?m)^a", 0},
		{"MID", 1},
	}
	for _, tt := range tests {
		res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: tt.pattern})
		require.NoError(t, err)
		text := resultText(t, res)
		require.Contains(t, text, fmt.Sprintf("matches: %d\n", tt.matches), tt.pattern)
		require.Contains(t, text, "approximate: true\n", tt.pattern)
	}

	// Short lines only: the result is exact
	res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: "^short$"})
	require.NoError(t, err)
	require.Contains(t, resultText(t, res), "matches: 1\n")
	short, err := svc.docs.CreateDocument("one\ntwo\n")
	require.NoError(t, err)
	res, _, err = svc.search(context.Background(), nil, SearchInput{KeyOrURL: short, Pattern: "^two$"})
	require.NoError(t, err)
	require.NotContains(t, resultText(t, res), "approximate")
}

func TestSearchLongLineBudget(t *testing.T) {
	// One huge line with a costly pattern: the budget is checked between the
	// pieces of the line, so the search stops on time instead of scanning it all
	// Scanning all of it takes seconds, many more with the race detector
	docs := handler.NewDocumentHandler(10, 16<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	key, err := docs.CreateDocument(strings.Repeat("\x1b", 8<<20))
	require.NoError(t, err)
	svc := newService(docs, 0)
	svc.searchBudget = 100 * time.Millisecond

	start := time.Now()
	res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: `[^x]{100}x`})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 3*time.Second)
	text := resultText(t, res)
	require.Contains(t, text, "truncated: time budget reached")
	require.Contains(t, text, "inside line 1, which is too long for this pattern")

	// A cancelled call stops between pieces too
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.searchBudget = time.Minute
	_, _, err = svc.search(ctx, nil, SearchInput{KeyOrURL: key, Pattern: `[^x]{100}x`})
	require.ErrorIs(t, err, context.Canceled)
}

func TestSearchPatternCap(t *testing.T) {
	svc, key := searchService(t, "x\n")
	res, _, err := svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: `(?:\w+\s*){1,500}Z`})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "at most 2000")

	res, _, err = svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: `[^x]{900}x`})
	require.NoError(t, err)
	require.False(t, res.IsError)
}

func TestToolMemoryBudget(t *testing.T) {
	// A streamed read charges a small fixed amount; when that does not fit the tool says the server is busy
	svc, key := searchService(t, strings.Repeat("line\n", 20000))
	svc.docs.ReadMemory = handler.NewMemoryBudget(16 << 20)
	var holds []func()
	for i, size := range []int64{4 << 20, 4 << 20, 4 << 20, 3 << 20} {
		hold, ok := svc.docs.ReadMemory.Acquire(fmt.Sprintf("192.0.2.%d", i+1), size)
		require.True(t, ok)
		holds = append(holds, hold)
	}

	for _, call := range []func() (*mcp.CallToolResult, any, error){
		func() (*mcp.CallToolResult, any, error) {
			return svc.read(context.Background(), nil, ReadInput{KeyOrURL: key})
		},
		func() (*mcp.CallToolResult, any, error) {
			return svc.search(context.Background(), nil, SearchInput{KeyOrURL: key, Pattern: "line"})
		},
	} {
		res, _, err := call()
		require.NoError(t, err)
		require.True(t, res.IsError)
		require.Contains(t, resultText(t, res), "server busy, retry")
	}

	// A small paste is read without the budget
	small, err := svc.docs.CreateDocument("line\n")
	require.NoError(t, err)
	res, _, err := svc.read(context.Background(), nil, ReadInput{KeyOrURL: small})
	require.NoError(t, err)
	require.False(t, res.IsError)

	holds[3]()
	res, _, err = svc.read(context.Background(), nil, ReadInput{KeyOrURL: key})
	require.NoError(t, err)
	require.False(t, res.IsError)
	for _, hold := range holds[:3] {
		hold()
	}
	require.Zero(t, svc.docs.ReadMemory.InUse())
	require.Zero(t, svc.reads.inUse())
	require.Zero(t, svc.searches.inUse())

	// A paste much larger than the charge reads fine: only the stream is charged
	big, err := svc.docs.CreateDocument(numberedLog(60000))
	require.NoError(t, err)
	svc.docs.ReadMemory = handler.NewMemoryBudget(4 * handler.ToolStreamMemory)
	res, _, err = svc.search(context.Background(), nil, SearchInput{KeyOrURL: big, Pattern: "^line 59999$"})
	require.NoError(t, err)
	require.Contains(t, resultText(t, res), "matches: 1\n")
}

func TestChunkedRequestOutOfMemory(t *testing.T) {
	// A request body is charged 6 times what has arrived: 503 when the upload budget runs out
	_, docs, ts := setup(t, 4*handler.LargeUpload)
	docs.UploadMemory = handler.NewMemoryBudget(64 * handler.UploadStep)
	var holds []func()
	for i, steps := range []int64{16, 16, 16, 15} {
		hold, ok := docs.UploadMemory.Acquire(fmt.Sprintf("192.0.2.%d", i+1), steps*handler.UploadStep)
		require.True(t, ok)
		holds = append(holds, hold)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"` +
		strings.Repeat("a", handler.LargeUpload) + `"}}}`
	resp := postMCP(t, ts.URL, strings.NewReader(body), -1)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "10", resp.Header.Get("Retry-After"))
	require.EqualValues(t, 63*handler.UploadStep, docs.UploadMemory.InUse())
	require.Zero(t, docs.UploadsInProgress())

	// A body of known size is charged as it arrives too, never up front
	resp = postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	for _, hold := range holds {
		hold()
	}

	// While it arrives, the charge is 6 times what came: 1.5 MiB is 9 MiB
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, pr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	head := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"`
	first := strings.Repeat("a", 3*handler.UploadStep/2-len(head))
	_, err := pw.Write([]byte(head + first))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return docs.UploadMemory.InUse() == 9*handler.UploadStep }, 2*time.Second, 5*time.Millisecond)
	_, err = pw.Write([]byte(`"}}}`))
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	require.Equal(t, http.StatusOK, <-done)

	resp = postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Zero(t, docs.UploadMemory.InUse())
}

func TestMaxLengthFitsBudget(t *testing.T) {
	docs := handler.NewDocumentHandler(10, 32<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))

	// No budget: the 4 MiB default and twice it plus the framing
	require.Equal(t, 4<<20, EffectiveMaxLength(0, docs))
	require.EqualValues(t, 8<<20+JSONOverhead, bodyLimit(4<<20, docs))

	// The default budget: one client may hold 32 MiB of uploads; 4 MiB charged 6 times fits,
	// and the body may be a sixth of that share
	docs.SetMemoryBudget(256 << 20)
	require.Equal(t, 4<<20, EffectiveMaxLength(0, docs))
	require.EqualValues(t, (32<<20)/handler.MCPBodyCost, bodyLimit(4<<20, docs))
	require.Equal(t, (32<<20)/handler.MCPBodyCost*4/5, EffectiveMaxLength(8<<20, docs))

	// A 300 MiB container has a 150 MiB budget: a sixth of 18.75 MiB
	docs.SetMemoryBudget(150 << 20)
	share := docs.UploadMemory.ClientMax()
	limit := EffectiveMaxLength(0, docs)
	require.EqualValues(t, share/handler.MCPBodyCost*4/5, limit)
	require.Less(t, limit, 4<<20)
	require.LessOrEqual(t, handler.MCPBodyCost*bodyLimit(limit, docs), share)
	require.GreaterOrEqual(t, bodyLimit(limit, docs), int64(limit)*5/4, "room for the JSON escaping")

	// A small budget lowers both further
	docs.SetMemoryBudget(32 << 20)
	require.EqualValues(t, (4<<20)/handler.MCPBodyCost*4/5, EffectiveMaxLength(1<<20, docs))
	require.EqualValues(t, (4<<20)/handler.MCPBodyCost, bodyLimit(1<<20, docs))
}

// noSpaceStore is a file store whose disk is always too full
type noSpaceStore struct{ storage.Storage }

func (noSpaceStore) CheckSpace(int64) error { return storage.ErrNoSpace }

func TestUploadNoSpace(t *testing.T) {
	docs := handler.NewDocumentHandler(10, 1024, noSpaceStore{storage.NewFileStorage(t.TempDir(), 0)}, keygenerator.NewRandomKeyGenerator(""))
	svc := newService(docs, 0)
	res, err := svc.upload(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{"content":"log"}`)}})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "low on disk space")
}

func TestBatchRejected(t *testing.T) {
	_, _, ts := setup(t, 1<<20)

	var items []string
	for i := 0; i < 20000; i++ {
		items = append(items, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, i))
	}
	for _, body := range []string{"[" + strings.Join(items, ",") + "]", " \r\n\t[]"} {
		resp := postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(data), `"code":-32600`)
		require.Contains(t, string(data), "batch")
	}

	// A single request with leading whitespace still works
	body := "  \n" + `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	resp := postMCP(t, ts.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), "upload_log")
}

func TestUploadQuotaMCP(t *testing.T) {
	session, docs, _ := setup(t, 1024)
	docs.Quota = handler.NewUploadQuota(100, time.Hour)

	upload(t, session, strings.Repeat("a", 60))
	text, res := call(t, session, "upload_log", map[string]any{"content": strings.Repeat("b", 60)})
	require.True(t, res.IsError)
	require.Contains(t, text, "upload quota for your address is used up")
	require.Contains(t, text, "retry in 3600 seconds")
	upload(t, session, strings.Repeat("c", 40))
}

// openRequest starts a POST from the client at ip whose body of the given
// announced size is still arriving. It returns the body writer and the status code, once there is one.
func openRequest(t *testing.T, url, ip string, size int64, head string) (*io.PipeWriter, chan int) {
	t.Helper()
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, url, pr)
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("X-Test-Client", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	_, err := pw.Write([]byte(head))
	require.NoError(t, err)
	t.Cleanup(func() { pw.CloseWithError(io.ErrClosedPipe) })
	return pw, done
}

func TestInflightPerClient(t *testing.T) {
	docs := handler.NewDocumentHandler(10, 1<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	docs.SetMemoryBudget(256 << 20)
	ts := clientServer(t, docs)

	// Four small requests from one client, whose bodies are still arriving
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	head, tail := body[:20], body[20:]
	var writers []*io.PipeWriter
	var results []chan int
	for range DefaultMaxInflightPerClient {
		pw, done := openRequest(t, ts.URL, "192.0.2.1", int64(len(body)), head)
		writers = append(writers, pw)
		results = append(results, done)
	}
	// Every body is charged as it arrives, small as it is
	require.Eventually(t, func() bool {
		return docs.UploadMemory.InUse() == DefaultMaxInflightPerClient*handler.ChargeUnit
	}, 2*time.Second, 5*time.Millisecond)

	// A fifth gets 429 with a JSON-RPC error and Retry-After
	resp := postMCPAs(t, ts.URL, "192.0.2.1", body)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "2", resp.Header.Get("Retry-After"))
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(data), `"error"`)
	require.Contains(t, string(data), "Too many MCP requests")

	// Another client is not held up
	resp = postMCPAs(t, ts.URL, "192.0.2.2", body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The four finish, and the client may send again
	for i, pw := range writers {
		_, err := pw.Write([]byte(tail))
		require.NoError(t, err)
		require.NoError(t, pw.Close())
		require.Equal(t, http.StatusOK, <-results[i])
	}
	resp = postMCPAs(t, ts.URL, "192.0.2.1", body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Zero(t, docs.UploadMemory.InUse())

	// -1 turns the limit off
	off := httptest.NewServer(NewHandler(docs, Options{MaxInflightPerClient: -1}))
	t.Cleanup(off.Close)
	for range 2 * DefaultMaxInflightPerClient {
		_, _ = openRequest(t, off.URL, "", int64(len(body)), head)
	}
	resp = postMCP(t, off.URL, strings.NewReader(body), int64(len(body)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// postMCPAs sends one raw JSON-RPC request from the client at ip
func postMCPAs(t *testing.T, url, ip, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("X-Test-Client", ip)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSmallRequestCharged(t *testing.T) {
	// A body under 1 MiB takes no upload slot but is charged 6 times what has
	// arrived, as the MCP library holds and decodes it whole
	docs := handler.NewDocumentHandler(10, 4<<20, storage.NewFileStorage(t.TempDir(), 0), keygenerator.NewRandomKeyGenerator(""))
	docs.UploadMemory = handler.NewMemoryBudget(64 << 20)
	docs.LimitUploads(1)
	ts := clientServer(t, docs)

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"upload_log","arguments":{"content":"`
	content := strings.Repeat("a", handler.LargeUpload-len(call)-200)
	body := call + content + `"}}}`
	half := len(body) / 2
	pw, done := openRequest(t, ts.URL, "192.0.2.1", int64(len(body)), body[:half])
	want := (int64(handler.MCPBodyCost*half) + handler.ChargeUnit - 1) / handler.ChargeUnit * handler.ChargeUnit
	require.Eventually(t, func() bool { return docs.UploadMemory.InUse() >= want-handler.ChargeUnit }, 2*time.Second, 5*time.Millisecond)
	require.LessOrEqual(t, docs.UploadMemory.InUse(), want)
	require.Zero(t, docs.UploadsInProgress())
	_, err := pw.Write([]byte(body[half:]))
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	require.Equal(t, http.StatusOK, <-done)
	require.Zero(t, docs.UploadMemory.InUse())

	// With the client's share used up, even a small request gets 503
	hold, ok := docs.UploadMemory.Acquire("192.0.2.1", 16<<20)
	require.True(t, ok)
	small := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	resp := postMCPAs(t, ts.URL, "192.0.2.1", body)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "10", resp.Header.Get("Retry-After"))
	resp = postMCPAs(t, ts.URL, "192.0.2.1", small)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// Another client still gets through
	resp = postMCPAs(t, ts.URL, "192.0.2.2", small)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hold()
	require.Zero(t, docs.UploadMemory.InUse())
}
