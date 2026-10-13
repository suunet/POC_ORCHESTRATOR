// Package ticket は、指示書（ticket.md）のステータスと項目の読み書き、実行の記録（runs/）を扱う。
package ticket

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Dir は、プロジェクトのルートから見たチケットの置き場所。File は指示書のファイル名。
const (
	Dir  = ".docs/_stash/tickets"
	File = "ticket.md"
)

// Status は、指示書のライフサイクルの状態。
type Status string

// 状態の一覧（設計の「ワークフローと状態管理」）。
const (
	Draft            Status = "draft"
	ReadyToPrepare   Status = "ready-to-prepare"
	ReadyToApprove   Status = "ready-to-approve"
	ReadyToImplement Status = "ready-to-implement"
	Implementing     Status = "implementing"
	ReadyForReview   Status = "ready-for-review"
	InUAT            Status = "in-uat"
	Feedback         Status = "feedback"
	Done             Status = "done"
	Cancelled        Status = "cancelled"
)

var statuses = []Status{Draft, ReadyToPrepare, ReadyToApprove, ReadyToImplement, Implementing,
	ReadyForReview, InUAT, Feedback, Done, Cancelled}

// Kind は、チケットの種別。実装は受入条件とテストケースを持つ。基盤は受入条件を持たず、
// テストケースは0件でよく、ホストでの確かめ（§4.5）を必ず持つ（M2.7）。
type Kind string

const (
	KindImplementation Kind = "実装"
	KindInfrastructure Kind = "基盤"
)

const statusPrefix = "ステータス:"

var (
	idPattern    = regexp.MustCompile(`^T-[0-9]{3,}$`)
	titlePattern = regexp.MustCompile(`^# (T-[0-9]{3,}) (.+)$`)
)

// Ticket は、読み込んだ指示書。
type Ticket struct {
	ID     string
	Title  string
	Status Status
	Kind   Kind
	Branch string
	Body   string // ファイルの全文
	Path   string // 指示書のファイルのパス

	root string // プロジェクトのルート
}

// Load は、root のプロジェクトの指示書 id を読み込んで検査する。誤りはまとめて返す。
func Load(root, id string) (*Ticket, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("チケットの ID %q は T-NNN の形でない", id)
	}
	path := filepath.Join(root, Dir, id, File)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("指示書を読めない: %w", err)
	}
	t, err := Parse(id, path, data)
	if err != nil {
		return nil, err
	}
	t.root = root
	return t, nil
}

// Parse は、読み込み済みの指示書の中身を検査する。path は表示と記録の置き場所の計算にだけ使い、読まない。
func Parse(id, path string, data []byte) (*Ticket, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("チケットの ID %q は T-NNN の形でない", id)
	}
	t := &Ticket{ID: id, Body: string(data), Path: path}
	if errs := t.parse(); len(errs) > 0 {
		return nil, fmt.Errorf("指示書 %s の内容が正しくない:\n%w", path, errors.Join(errs...))
	}
	return t, nil
}

func (t *Ticket) parse() []error {
	var errs []error
	var statusLines int
	for _, line := range strings.Split(t.Body, "\n") {
		if m := titlePattern.FindStringSubmatch(line); m != nil && t.Title == "" {
			if m[1] != t.ID {
				errs = append(errs, fmt.Errorf("見出しの ID %s がチケットの ID %s と違う", m[1], t.ID))
			}
			t.Title = strings.TrimSpace(m[2])
		}
		if v, ok := strings.CutPrefix(line, statusPrefix); ok {
			statusLines++
			t.Status = Status(strings.TrimSpace(v))
		}
		if v, ok := tableValue(line, "種別"); ok {
			t.Kind = Kind(v)
		}
		if v, ok := tableValue(line, "ブランチ"); ok {
			t.Branch = v
		}
	}
	if t.Title == "" {
		errs = append(errs, errors.New("見出し（# T-NNN タイトル）がない"))
	}
	switch {
	case statusLines == 0:
		errs = append(errs, errors.New("ステータスの行がない"))
	case statusLines > 1:
		errs = append(errs, errors.New("ステータスの行が複数ある"))
	case !validStatus(t.Status):
		errs = append(errs, fmt.Errorf("ステータス %q は知らない状態", t.Status))
	}
	if t.Kind != KindImplementation && t.Kind != KindInfrastructure {
		errs = append(errs, fmt.Errorf("種別 %q は「実装」か「基盤」でない", t.Kind))
	}
	if t.Branch == "" {
		errs = append(errs, errors.New("ブランチの欄がない"))
	}
	return errs
}

// tableValue は、「| 項目 | 値 |」の行から、項目が key のときの値を返す。
func tableValue(line, key string) (string, bool) {
	cells := splitRow(line)
	if len(cells) != 2 || strings.TrimSpace(cells[0]) != key {
		return "", false
	}
	return strings.TrimSpace(cells[1]), true
}

func validStatus(s Status) bool {
	for _, v := range statuses {
		if v == s {
			return true
		}
	}
	return false
}

// SetStatus は、ファイルのステータスが from であることを確かめてから to に書き換える。
// ステータスの行のほかは変えない。書き換えは一時ファイルに書いてから名前を変える。
func (t *Ticket) SetStatus(from, to Status) error {
	if !validStatus(to) {
		return fmt.Errorf("ステータス %q は知らない状態", to)
	}
	current, err := Load(t.root, t.ID)
	if err != nil {
		return err
	}
	if current.Status != from {
		return fmt.Errorf("指示書 %s のステータスが %s ではなく %s になっている", t.ID, from, current.Status)
	}
	lines := strings.Split(current.Body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, statusPrefix) {
			lines[i] = statusPrefix + " " + string(to)
		}
	}
	body := strings.Join(lines, "\n")
	if err := writeFileAtomic(t.Path, []byte(body)); err != nil {
		return fmt.Errorf("指示書 %s のステータスを書き換えられない: %w", t.ID, err)
	}
	t.Status, t.Body = to, body
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
