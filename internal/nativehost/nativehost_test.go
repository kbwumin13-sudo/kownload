package nativehost

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	status    int
	body      string
	calls     int
	available bool
	wakeError error
	last      Request
}

func (f *fixture) Available(context.Context) bool { return f.available }
func (f *fixture) Post(_ context.Context, _ string, r Request) (int, []byte, error) {
	f.calls++
	f.last = r
	return f.status, []byte(f.body), nil
}
func (f *fixture) Ensure(context.Context, bool) error { return f.wakeError }
func runFixture(f *fixture, method string) Response {
	return (Dispatcher{Client: f, Wakeup: f}).Dispatch(context.Background(), []byte(`{"method":"`+method+`","operationId":"op-1","params":{"method":"POST","path":"/api/v1/tasks","data":{}}}`))
}
func TestB02CreateHTTP500(t *testing.T) {
	f := &fixture{status: 500, body: `{"code":1000,"msg":"Cookie: secret; https://example.org/private?token=secret","data":{"path":"/Users/private"}}`}
	r := runFixture(f, "create")
	if r.Code != 1 || r.HTTPStatus == nil || *r.HTTPStatus != 500 || r.BusinessCode == nil || *r.BusinessCode != 1000 || r.Error != "http_error" || f.calls != 1 {
		t.Fatalf("incorrect B02 outcome: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "/Users/") {
		t.Fatal("sensitive error echoed")
	}
}
func TestOutcomesAndNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name, method, body, kind string
		status                   int
		success                  bool
	}{
		{"accepted", "create", `{"code":0,"operationId":"op-1","data":null,"submission":{"state":"accepted","taskIds":[]}}`, "", 202, true},
		{"created", "forward", `{"code":0,"operationId":"op-1","data":"t1","submission":{"state":"created","taskIds":["t1"]}}`, "", 200, true},
		{"business", "forward", `{"code":1000,"msg":"private"}`, "business_error", 200, false},
		{"false HTTP success", "create", `{"code":0}`, "http_error", 500, false},
		{"empty", "create", ``, "invalid_response", 200, false},
		{"bad JSON", "forward", `broken`, "invalid_response", 200, false},
		{"missing submission", "create", `{"code":0,"operationId":"op-1"}`, "invalid_response", 200, false},
		{"empty IDs", "forward", `{"code":0,"operationId":"op-1","submission":{"state":"created","taskIds":[]}}`, "invalid_response", 200, false},
		{"wrong ID", "create", `{"code":0,"operationId":"other","submission":{"state":"accepted","taskIds":[]}}`, "invalid_response", 200, false},
		{"missing code", "create", `{"operationId":"op-1"}`, "invalid_response", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fixture{status: tc.status, body: tc.body}
			for i := 0; i < 2; i++ {
				r := runFixture(f, tc.method)
				if (r.Code == 0) != tc.success || r.Error != tc.kind {
					t.Fatalf("%+v", r)
				}
				if tc.success && r.Submission == nil {
					t.Fatal("missing submission")
				}
			}
			if f.calls != 2 || f.last.OperationID != "op-1" {
				t.Fatal("host deduplicated/retried or lost ID")
			}
		})
	}
}
func TestLegacyEnvelopeAndValidation(t *testing.T) {
	f := &fixture{status: 200, body: `{"code":0,"data":{"value":1}}`}
	d := Dispatcher{Client: f, Wakeup: f}
	r := d.Dispatch(context.Background(), []byte(`{"method":"forward","params":{"method":"GET","path":"/api/v1/info"}}`))
	if r.Code != 0 || string(r.Data.(json.RawMessage)) != f.body {
		t.Fatal("old successful business data changed")
	}
	for _, body := range []string{`{"method":"create","operationId":"bad\r\n"}`, `{"method":"forward","params":{"method":"POST","path":"https://example.org/"}}`, `{"method":"forward","params":{"method":"POST","path":"/api/v1/%2e%2e/private"}}`, `{"method":"create","params":null}`, `{"method":"create","meta":{"silent":"yes"},"params":{}}`} {
		if d.Dispatch(context.Background(), []byte(body)).Code == 0 {
			t.Fatal("accepted invalid request")
		}
	}
	if r := d.Dispatch(context.Background(), []byte(`{"method":"unknown"}`)); r.Error != "unknown_method" {
		t.Fatal(r)
	}
	if r := d.Dispatch(context.Background(), []byte(`{"method":"ping"}`)); r.Code != 0 || r.Data != false {
		t.Fatal(r)
	}
	f.wakeError = errors.New("private")
	if r := runFixture(f, "create"); r.Error != "wakeup_failed" {
		t.Fatal(r)
	}
}

type chunks struct{ io.Reader }

func (c chunks) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return c.Reader.Read(p)
}
func requestFrame(body string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.NativeEndian, uint32(len(body)))
	b.WriteString(body)
	return b.Bytes()
}
func TestFrames(t *testing.T) {
	input := append(requestFrame(`{"method":"ping"}`), requestFrame(`{"method":"wakeup"}`)...)
	var out bytes.Buffer
	calls := 0
	if err := Serve(chunks{bytes.NewReader(input)}, &out, func([]byte) Response { calls++; return Response{Data: true} }); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
	for i := 0; i < 2; i++ {
		body, err := ReadFrame(&out)
		if err != nil || !bytes.Contains(body, []byte(`"code":0`)) {
			t.Fatal(err)
		}
	}
	for _, data := range [][]byte{{1, 0}, requestFrame(`not JSON`), requestFrame(string([]byte{0xff})), {0, 0, 0, 0}, {255, 255, 255, 255}, requestFrame(`{}`)[:5]} {
		if _, err := ReadFrame(bytes.NewReader(data)); err == nil {
			t.Fatal("bad frame accepted")
		}
	}
	out.Reset()
	if err := WriteFrame(&out, Response{Data: strings.Repeat("x", MaxOutput)}); err != nil {
		t.Fatal(err)
	}
	body, _ := ReadFrame(&out)
	if !bytes.Contains(body, []byte(`"error":"invalid_response"`)) {
		t.Fatal("oversized output")
	}
}
func TestUnixIPCStatusHeadersAndNoRedirect(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "knh-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "rpc.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Kownload-Operation-Id") != "op-1" || r.Header.Get("X-Kownload-Host-Meta") != `{"silent":true}` || r.Header.Get("X-Gopeed-Host-Meta") == "" {
			t.Error("lost headers")
		}
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "/create")
			w.WriteHeader(307)
			return
		}
		w.WriteHeader(500)
		w.Write([]byte(`{"code":1000}`))
	})}
	go server.Serve(listener)
	defer server.Close()
	c := UnixClient{Socket: socket}
	if !c.Available(context.Background()) {
		t.Fatal("probe")
	}
	for _, path := range []string{"/create", "/redirect"} {
		status, _, err := c.Post(context.Background(), path, Request{OperationID: "op-1", Meta: map[string]any{"silent": true}, Params: json.RawMessage(`{}`)})
		if err != nil || status < 300 {
			t.Fatal(status, err)
		}
	}
	if calls != 2 {
		t.Fatal("POST replayed")
	}
}
func TestWakeupAndOrigins(t *testing.T) {
	f := &fixture{}
	opens := 0
	w := AppWakeup{Client: f, Scheme: "kownload", Open: func(_ context.Context, uri string) error {
		opens++
		if uri != "kownload:?hidden=true" {
			t.Fatal(uri)
		}
		f.available = true
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if w.Ensure(ctx, true) != nil || w.Ensure(ctx, true) != nil || opens != 1 {
		t.Fatal("wakeup repetitions")
	}
	if !AllowedOrigin("chrome-extension://jcalkpjhhaachliihlkojhmafniepnoj/") || AllowedOrigin("chrome-extension://untrusted/") || AllowedOrigin("") {
		t.Fatal("origin boundary")
	}
}
