package monitor

import (
	"encoding/json"
	"net/http"
)

// Handler は、監視の UI の口を返す。口は GET / （埋め込みの HTML 1枚）と GET /api/status（JSON）の2つだけ。
// GET 以外のメソッドは 405、知らないパスは 404。読み取り専用で、要求から読み先のパスを受け取らない。
func Handler(m *Monitor, page []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			h.Set("Allow", http.MethodGet)
			http.Error(w, "GET だけを受け付ける", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/":
			// NOTE: 外部の JS・CSS・フォントは読み込まない（ページの中の script と style だけを許す）
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
			h.Set("Content-Type", "text/html; charset=utf-8")
			w.Write(page)
		case "/api/status":
			st, err := m.Status()
			if err != nil {
				http.Error(w, "記録を読めない: "+err.Error(), http.StatusInternalServerError)
				return
			}
			h.Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(st)
		default:
			http.NotFound(w, r)
		}
	})
}
