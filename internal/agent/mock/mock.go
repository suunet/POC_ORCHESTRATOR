// Package mock は、テスト用のダミーの AI。筋書き（step ごとの出力の並び）に従って出力を書くだけで、外へは通信しない。
//
// 自動のテストでは engine に直接渡す。コマンドからは隠しの指定でだけ選べる（使い方の表示には載せない）。
package mock

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gateflow/internal/agent"
	"gateflow/internal/container"
)

// Entry は、step の1回分の出力。Result は /out/result.json に、Files は作業ツリー（/work）に書く。
type Entry struct {
	Result json.RawMessage   `json:"result"`
	Files  map[string]string `json:"files,omitempty"`
}

// Scenario は、筋書き。step ごとに、呼ばれた順に使う出力を並べる。尽きたら最後の出力を繰り返す。
type Scenario struct {
	Steps map[string][]Entry `json:"steps"`
}

// ParseScenario は、筋書きのファイルを読み込んで検査する。
func ParseScenario(data []byte) (*Scenario, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("筋書きの形式が正しくない: %w", err)
	}
	var errs []error
	if len(s.Steps) == 0 {
		errs = append(errs, errors.New("筋書きに steps がない"))
	}
	for step, entries := range s.Steps {
		if len(entries) == 0 {
			errs = append(errs, fmt.Errorf("筋書きの %s に出力がない", step))
		}
		for i, e := range entries {
			if len(e.Result) == 0 || string(e.Result) == "null" {
				errs = append(errs, fmt.Errorf("筋書きの %s[%d] に result がない", step, i))
			}
			for p := range e.Files {
				if !filepath.IsLocal(p) || strings.ContainsAny(p, "'\n") {
					errs = append(errs, fmt.Errorf("筋書きの %s[%d] のファイル %q は作業ツリーの中のパスでない", step, i, p))
				}
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &s, nil
}

// Agent は、筋書きに従うエージェント。
type Agent struct {
	Scenario *Scenario

	mu    sync.Mutex
	calls map[string]int
}

func (*Agent) Name() string { return "mock" }

// Command は、筋書きの次の出力を書くスクリプトを返す。筋書きにない step は何も書かない（出力の不正になる）。
func (a *Agent) Command(req agent.Request) ([]string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var entries []Entry
	if a.Scenario != nil {
		entries = a.Scenario.Steps[req.Step]
	}
	if len(entries) == 0 {
		return []string{"sh", "-c", "true"}, ""
	}
	if a.calls == nil {
		a.calls = map[string]int{}
	}
	e := entries[min(a.calls[req.Step], len(entries)-1)]
	a.calls[req.Step]++

	var b strings.Builder
	b.WriteString("set -e\n")
	paths := make([]string, 0, len(e.Files))
	for p := range e.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	write := func(target, content string) {
		fmt.Fprintf(&b, "mkdir -p \"$(dirname '%s')\"\nprintf '%%s' '%s' | base64 -d > '%s'\n",
			target, base64.StdEncoding.EncodeToString([]byte(content)), target)
	}
	for _, p := range paths {
		write("/work/"+filepath.ToSlash(p), e.Files[p])
	}
	write("/out/result.json", string(e.Result))
	return []string{"sh", "-c", b.String()}, ""
}

func (*Agent) Hosts() []string { return nil }

func (*Agent) AuthMount() container.Mount {
	return container.Mount{Type: container.Volume, Source: "gateflow-auth-mock", Target: "/auth"}
}

func (*Agent) Env() map[string]string { return map[string]string{} }

func (*Agent) LoginCommand() []string { return []string{"true"} }
