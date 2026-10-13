package monitor

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
)

// Handler は、監視の UI の口を返す。口は GET / （埋め込みの HTML 1枚）、GET /api/status、GET /api/ticket?id=、
// GET /api/source?path= の4つだけ（M2.8 で後の2つを足した）。GET 以外のメソッドは 405、知らないパスは 404。
// 読み取り専用で、/api/source が読めるのは tickets の下とカタログだけ（os.Root で範囲の外へ出ない）。
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
			writeJSON(w, st)
		case "/api/ticket":
			d, err := m.Ticket(r.URL.Query().Get("id"))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				http.NotFound(w, r)
			case err != nil:
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				writeJSON(w, d)
			}
		case "/api/source":
			f, err := m.Source(r.URL.Query().Get("path"))
			switch {
			case errors.Is(err, ErrOutside):
				http.Error(w, err.Error(), http.StatusForbidden)
			case errors.Is(err, fs.ErrNotExist):
				http.NotFound(w, r)
			case err != nil:
				http.Error(w, err.Error(), http.StatusBadRequest)
			default:
				writeJSON(w, f)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// NOTE: 応答の途中で相手が切れても、読み取り専用なので記録するものはない
	json.NewEncoder(w).Encode(v)
}
