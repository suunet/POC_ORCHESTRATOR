package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// StateFile は、実行の記録の中の、実行中の状態のファイル名。
const StateFile = "state.json"

// ProxyLogFile は、実行の記録の中の、プロキシの判定の記録のファイル名。実行の間に逐次書く。
const ProxyLogFile = "proxy.log"

// State は、実行中の状態（状態の2層管理の下の層）。書くのは gateflow だけ。
type State struct {
	Ticket    string            `json:"ticket"`
	Workflow  string            `json:"workflow"`
	Step      string            `json:"step,omitempty"`   // 今の step
	Steps     int               `json:"steps"`            // この実行の step の数
	Loops     map[string]int    `json:"loops,omitempty"`  // ループの種類ごとの往復の数
	Prints    map[string]string `json:"prints,omitempty"` // 進展なしの判定に使う直前の指紋
	Findings  []string          `json:"findings,omitempty"`
	Feedback  string            `json:"feedback,omitempty"`  // 次の step に渡す差し戻しの内容（ファイルの場所）
	DevResult string            `json:"devResult,omitempty"` // 最後の P5 の result.json（P5後に渡す）
	VerifyLog string            `json:"verifyLog,omitempty"` // 最後の検証の出力（P5後に渡す）
	Commit    string            `json:"commit,omitempty"`    // 最後のコミット
	Status    string            `json:"status"`              // gateflow が最後に書いたステータス（手での書き換えの検出用）
	Stopped   *Stop             `json:"stopped,omitempty"`
	Resume    string            `json:"resume,omitempty"`  // 再開の位置（次に実行する step）
	Resumed   bool              `json:"resumed,omitempty"` // resume で続けた後か
}

// Stop は、止まった理由。
type Stop struct {
	Reason string `json:"reason"` // gate・human・limit-steps・limit-loops・no-progress・timeout・invalid-output など
	Detail string `json:"detail,omitempty"`
}

// Save は、dir/state.json に書く。一時ファイルに書いてから名前を変える。
func (s *State) Save(dir string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, StateFile))
}

// LoadState は、dir/state.json を読む。
func LoadState(dir string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		return nil, err
	}
	return DecodeState(data)
}

// DecodeState は、state.json の中身を読む。
func DecodeState(data []byte) (*State, error) {
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s の形式が正しくない: %w", StateFile, err)
	}
	if s.Loops == nil {
		s.Loops = map[string]int{}
	}
	if s.Prints == nil {
		s.Prints = map[string]string{}
	}
	return &s, nil
}

// LatestState は、runs の下で最も新しい（名前の順で最後の）state.json のある記録を返す。
func LatestState(runs string) (*State, string, error) {
	entries, err := os.ReadDir(runs)
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		dir := filepath.Join(runs, n)
		if s, err := LoadState(dir); err == nil {
			return s, dir, nil
		}
	}
	return nil, "", errors.New("実行の記録がない")
}
