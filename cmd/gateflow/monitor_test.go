package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 監視の UI は 127.0.0.1 だけで待ち受け、既定のポートは 7420。--port で変える。
func TestMonitorServer_Addr(t *testing.T) {
	root := project(t, actorTable)
	for _, tc := range []struct {
		args []string
		addr string
	}{
		{[]string{"--project", root}, "127.0.0.1:7420"},
		{[]string{"--project", root, "--port", "8123"}, "127.0.0.1:8123"},
	} {
		srv, err := newMonitorServer(tc.args, io.Discard)
		if err != nil || srv.Addr != tc.addr || srv.ReadHeaderTimeout == 0 {
			t.Errorf("%v: srv = %+v, err = %v", tc.args, srv, err)
		}
	}
	for _, args := range [][]string{{"--project", root, "--port", "0"}, {"--project", root, "--port", "70000"}, {"--project", root, "extra"}} {
		if _, err := newMonitorServer(args, io.Discard); err == nil {
			t.Errorf("%v を受け付けた", args)
		}
	}
}

// 埋め込みの HTML と /api/status を返す。チケットの状態は status と同じ判定で出る。
func TestMonitorServer_PageAndStatus(t *testing.T) {
	root := project(t, actorTable)
	writeTicket(t, root, "T-001", "ready-to-approve")
	srv, err := newMonitorServer([]string{"--project", root}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("/api/status")) || bytes.Contains(rec.Body.Bytes(), []byte("innerHTML =")) {
		t.Errorf("/ = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var st struct {
		Cards []struct{ Kind, Ticket, Next string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || len(st.Cards) != 1 || st.Cards[0].Kind != "approve" ||
		!strings.Contains(st.Cards[0].Next, "gateflow approve T-001") {
		t.Errorf("/api/status = %s（%v）", rec.Body, err)
	}
}
