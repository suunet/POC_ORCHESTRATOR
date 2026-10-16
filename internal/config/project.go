// Package config は、プロジェクトの設定（.gateflow/project.json）を読み込み、書式を検査する。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// FormatVersion は、このgateflowが読める project.json の書式のバージョン。
// 1 で name・branch・agent・deps・verify を、2 で workflow・limits・issue を足した。古い書式は自動で変換せず拒否する。
const FormatVersion = 2

// ProjectFile は、プロジェクトのルートから見た設定ファイルの場所。
const ProjectFile = ".gateflow/project.json"

// 定義の見つけ方。
const (
	FindTableFirstColumn = "table-first-column"
	findTableColumn      = "table-column:"
)

// Project は project.json の中身。
type Project struct {
	FormatVersion   *int     `json:"formatVersion"`
	GateflowVersion string   `json:"gateflowVersion,omitempty"`
	Name            string   `json:"name"`
	Branch          *Branch  `json:"branch"`
	Agent           *Agent   `json:"agent"`
	Deps            *Deps    `json:"deps,omitempty"`
	Verify          *Verify  `json:"verify"`
	Workflow        string   `json:"workflow,omitempty"`
	Limits          *Limits  `json:"limits,omitempty"`
	Issue           *Issue   `json:"issue,omitempty"`
	IDs             *IDs     `json:"ids"`
	Policies        []Policy `json:"policies"`
	// Contracts は、外から見える契約の ID（例：NFR-06、SCR-*。末尾の * は前方一致）。
	// ARB の裁定がこれに触れたら、gateflow が人へ回させる（M2.6 の Q-05）。
	Contracts []string `json:"contracts,omitempty"`
}

// IsContract は、id が契約の ID の一覧に当たるかを返す。
func (p *Project) IsContract(id string) bool {
	for _, c := range p.Contracts {
		if prefix, ok := strings.CutSuffix(c, "*"); ok {
			if strings.HasPrefix(id, prefix) {
				return true
			}
		} else if id == c {
			return true
		}
	}
	return false
}

// Branch は、ブランチの設定。作業ブランチの名前は指示書の欄で決める。
type Branch struct {
	// Main は、タスクごとの clone を作る元のブランチ（人が承認した内容）。
	Main string `json:"main"`
}

// Agent は、エージェントのコンテナの設定。パスはプロジェクトのルートからの相対パス。
type Agent struct {
	// Dockerfile は、gateflow の土台のイメージに言語の道具を足す Dockerfile。
	Dockerfile string `json:"dockerfile"`
	// EnvTemplate は、外部への送信を OFF にした .env のひな形。clone の .env の位置に読み取り専用で置く。
	EnvTemplate string `json:"envTemplate"`
	// Allow は、AI サービスのほかに通信を許すホスト（host:port）。パッケージの取得先は入れない。
	Allow []string `json:"allow"`
}

// Deps は、依存の取得の設定。
//
// NOTE: Command は、人が管理するこのファイルの値を、人が承認した main の clone に対してだけ、
// 準備用のコンテナ（エージェントは動かない）で実行する。エージェントが書き換えた clone には実行しない。
type Deps struct {
	// Command は、依存を取得するコマンド（例：go mod download）。
	Command string `json:"command"`
	// CacheEnv は、取得した依存の置き場所を渡す環境変数の名前（例：GOMODCACHE）。
	CacheEnv string `json:"cacheEnv"`
}

// Limits は、暴走防止の上限。ない項目（0）は設計の初期値を使う。
type Limits struct {
	Steps       int `json:"steps,omitempty"`       // チケット1回の実行全体の step の数
	Loops       int `json:"loops,omitempty"`       // ループの往復（種類ごと）
	IdleMinutes int `json:"idleMinutes,omitempty"` // エージェントの無反応
	RunMinutes  int `json:"runMinutes,omitempty"`  // エージェントの1回の実行時間
	// ArbitrateLoops は、ARB の裁定から P2 へ戻る往復（M2.6）。超えたら、残りを人へ回す
	ArbitrateLoops int `json:"arbitrateLoops,omitempty"`
}

// 上限の初期値（設計の「暴走防止と通知」）。
var defaultLimits = Limits{Steps: 20, Loops: 3, IdleMinutes: 10, RunMinutes: 60, ArbitrateLoops: 2}

// EffectiveLimits は、設定の値と初期値を合わせた上限を返す。
func (p *Project) EffectiveLimits() Limits {
	l := defaultLimits
	if p.Limits != nil {
		for _, f := range []struct {
			dst *int
			v   int
		}{
			{&l.Steps, p.Limits.Steps}, {&l.Loops, p.Limits.Loops},
			{&l.IdleMinutes, p.Limits.IdleMinutes}, {&l.RunMinutes, p.Limits.RunMinutes},
			{&l.ArbitrateLoops, p.Limits.ArbitrateLoops},
		} {
			if f.v > 0 {
				*f.dst = f.v
			}
		}
	}
	return l
}

// WorkflowName は、使う遷移表の名前（既定は aidd-impl）。
func (p *Project) WorkflowName() string {
	if p.Workflow == "" {
		return "aidd-impl"
	}
	return p.Workflow
}

// Issue は、外部のチケット管理への参照の書式。外部の API は使わず、課題の番号は指示書の欄に人が書く。
type Issue struct {
	Tracker   string `json:"tracker"`             // github・backlog・none
	CommitRef string `json:"commitRef,omitempty"` // コミットメッセージの末尾に足す書式（例：#{id}）
	PRRef     string `json:"prRef,omitempty"`     // PR の本文の書式（例：Refs #{id}）。gateflow は表示だけ
}

// Format は、書式 format の {id} を id に置き換える。
func (i *Issue) Format(format, id string) string {
	return strings.ReplaceAll(format, "{id}", id)
}

// Verify は、検証コマンドの対応表。どれも必須のレベルで、検証用のコンテナで実行する。
type Verify struct {
	Build string `json:"build"`
	Test  string `json:"test"`
	Lint  string `json:"lint"`
}

// IDs は、ID整合チェックの設定。
type IDs struct {
	// Scan は、参照を探すファイルまたはディレクトリ（ルートからの相対パス）。定義のファイルは常に含む。
	Scan []string `json:"scan,omitempty"`
	// DeprecatedMarker は、定義の行のセルがこの文字列で始まるとき、その定義を廃止とみなす印。
	DeprecatedMarker string   `json:"deprecatedMarker,omitempty"`
	Prefixes         []Prefix `json:"prefixes"`
	// Tests は、テストの置き場（ルートからの相対パスのディレクトリ）。受入条件の ID を持つテストを探す（M3）
	Tests []string `json:"tests,omitempty"`
	// TestMethods は、受入条件の「確認の方法」のうちテストとみなす値（書かなければ「シナリオテスト」「単体テスト」）
	TestMethods []string `json:"testMethods,omitempty"`
}

// DefaultTestMethods は、受入条件の確認の方法のうち、テストとみなす値の既定。
var DefaultTestMethods = []string{"シナリオテスト", "単体テスト"}

// Methods は、テストとみなす確認の方法を返す。
func (i IDs) Methods() []string {
	if len(i.TestMethods) > 0 {
		return i.TestMethods
	}
	return DefaultTestMethods
}

// Prefix は、1つのIDのプレフィックスの定義の見つけ方。
type Prefix struct {
	Prefix     string    `json:"prefix"`
	Pattern    string    `json:"pattern"`
	Files      []string  `json:"files"`
	Find       string    `json:"find"`
	NameColumn string    `json:"nameColumn,omitempty"`
	Children   *Children `json:"children,omitempty"`
	// MethodColumn は、受入条件の「確認の方法」の列。これがあるプレフィックスの定義は、テストがなければ必須の指摘（M3）
	MethodColumn string `json:"methodColumn,omitempty"`
	// VerifyColumn は、非機能要求の「確かめ方」の列。空か「—」なら推奨の指摘（M3）
	VerifyColumn string `json:"verifyColumn,omitempty"`
}

// FindColumn は、find が table-column:<列名> のときの列名を返す。それ以外は空。
func (p Prefix) FindColumn() string {
	return strings.TrimPrefix(p.Find, findTableColumn)
}

// IsTableColumn は、find が table-column:<列名> の形かを返す。
func (p Prefix) IsTableColumn() bool {
	return strings.HasPrefix(p.Find, findTableColumn)
}

// Children は、親のIDの下の子のID（例：STM-01.下書き）の定義の見つけ方。
// 表のうち parentColumn と nameColumn の両方を持つものの各行を、親のIDと子の名前の組として定義する。
type Children struct {
	Separator    string   `json:"separator"`
	Files        []string `json:"files"`
	ParentColumn string   `json:"parentColumn"`
	NameColumn   string   `json:"nameColumn"`
}

// Policy は、規約の目次の1行。
type Policy struct {
	Path    string   `json:"path"`
	Roles   []string `json:"roles"`
	Purpose string   `json:"purpose,omitempty"`
}

var (
	validRoles  = map[string]bool{"dev": true, "bj": true, "arb": true}
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	hostPort    = regexp.MustCompile(`^[A-Za-z0-9.-]+:[0-9]{1,5}$`)
	envName     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// LoadProject は、root/.gateflow/project.json を読み込んで検査する。誤りはまとめて返す。
func LoadProject(root string) (*Project, error) {
	path := filepath.Join(root, ProjectFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s を読めない: %w", ProjectFile, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Project
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s の形式が正しくない: %w", ProjectFile, err)
	}
	if errs := p.validate(); len(errs) > 0 {
		return nil, fmt.Errorf("%s の内容が正しくない:\n%w", ProjectFile, errors.Join(errs...))
	}
	return &p, nil
}

func (p *Project) validate() []error {
	var errs []error
	switch {
	case p.FormatVersion == nil:
		errs = append(errs, errors.New("formatVersion がない"))
	case *p.FormatVersion != FormatVersion:
		errs = append(errs, fmt.Errorf("formatVersion %d には対応していない（対応は %d）", *p.FormatVersion, FormatVersion))
	}
	switch {
	case p.Name == "":
		errs = append(errs, errors.New("name がない"))
	case !namePattern.MatchString(p.Name):
		errs = append(errs, fmt.Errorf("name %q は英小文字・数字・ハイフンでない", p.Name))
	}
	if p.Branch == nil {
		errs = append(errs, errors.New("branch がない"))
	} else if p.Branch.Main == "" {
		errs = append(errs, errors.New("branch.main がない"))
	}
	if p.Agent == nil {
		errs = append(errs, errors.New("agent がない"))
	} else {
		errs = append(errs, p.Agent.validate()...)
	}
	if p.Deps != nil {
		if p.Deps.Command == "" {
			errs = append(errs, errors.New("deps.command がない"))
		}
		if !envName.MatchString(p.Deps.CacheEnv) {
			errs = append(errs, fmt.Errorf("deps.cacheEnv %q は環境変数の名前でない", p.Deps.CacheEnv))
		}
	}
	if p.Verify == nil {
		errs = append(errs, errors.New("verify がない"))
	} else {
		for _, c := range []struct{ name, cmd string }{{"build", p.Verify.Build}, {"test", p.Verify.Test}, {"lint", p.Verify.Lint}} {
			if c.cmd == "" {
				errs = append(errs, fmt.Errorf("verify.%s がない", c.name))
			}
		}
	}
	if p.Workflow != "" && !namePattern.MatchString(p.Workflow) {
		errs = append(errs, fmt.Errorf("workflow %q は遷移表の名前（英小文字・数字・ハイフン）でない", p.Workflow))
	}
	if l := p.Limits; l != nil {
		for _, f := range []struct {
			name string
			v    int
		}{{"steps", l.Steps}, {"loops", l.Loops}, {"idleMinutes", l.IdleMinutes}, {"runMinutes", l.RunMinutes}, {"arbitrateLoops", l.ArbitrateLoops}} {
			if f.v < 0 {
				errs = append(errs, fmt.Errorf("limits.%s %d は負の数", f.name, f.v))
			}
		}
		if l.Steps == 0 && l.Loops == 0 && l.IdleMinutes == 0 && l.RunMinutes == 0 && l.ArbitrateLoops == 0 {
			errs = append(errs, errors.New("limits に値がない（limits.steps などを書くか、limits ごと消す）"))
		}
	}
	for i, c := range p.Contracts {
		if strings.TrimSuffix(c, "*") == "" {
			errs = append(errs, fmt.Errorf("contracts[%d] が空", i))
		}
	}
	if i := p.Issue; i != nil {
		switch i.Tracker {
		case "github", "backlog", "none":
		default:
			errs = append(errs, fmt.Errorf("issue.tracker %q は github・backlog・none のどれでもない", i.Tracker))
		}
		for _, f := range []struct{ name, v string }{{"commitRef", i.CommitRef}, {"prRef", i.PRRef}} {
			if f.v != "" && !strings.Contains(f.v, "{id}") {
				errs = append(errs, fmt.Errorf("issue.%s %q に {id} がない", f.name, f.v))
			}
		}
	}
	if p.IDs == nil {
		errs = append(errs, errors.New("ids がない"))
	} else {
		errs = append(errs, p.IDs.validate()...)
	}
	if p.Policies == nil {
		errs = append(errs, errors.New("policies がない"))
	}
	for i, pol := range p.Policies {
		if pol.Path == "" {
			errs = append(errs, fmt.Errorf("policies[%d]: path がない", i))
		}
		for _, r := range pol.Roles {
			if !validRoles[r] {
				errs = append(errs, fmt.Errorf("policies[%d]: roles の %q は dev・bj・arb のどれでもない", i, r))
			}
		}
	}
	return errs
}

func (a *Agent) validate() []error {
	var errs []error
	for _, f := range []struct{ name, path string }{{"agent.dockerfile", a.Dockerfile}, {"agent.envTemplate", a.EnvTemplate}} {
		if err := checkRelPath(f.path); err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", f.name, f.path, err))
		}
	}
	for i, h := range a.Allow {
		if !hostPort.MatchString(h) {
			errs = append(errs, fmt.Errorf("agent.allow[%d] %q は host:port の形でない", i, h))
		}
	}
	return errs
}

// checkRelPath は、パスがプロジェクトのルートからの相対パスで、ルートの外を指さないことを確かめる。
func checkRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("パスがない")
	case filepath.IsAbs(p):
		return errors.New("絶対パスは使えない")
	case !filepath.IsLocal(p):
		return errors.New("プロジェクトの外を指している")
	}
	return nil
}

func (ids *IDs) validate() []error {
	var errs []error
	seen := map[string]bool{}
	for i, pre := range ids.Prefixes {
		at := fmt.Sprintf("ids.prefixes[%d]（%s）", i, pre.Prefix)
		if pre.Prefix == "" {
			errs = append(errs, fmt.Errorf("%s: prefix がない", at))
		} else if seen[pre.Prefix] {
			errs = append(errs, fmt.Errorf("%s: prefix が重複している", at))
		}
		seen[pre.Prefix] = true
		if _, err := regexp.Compile(pre.Pattern); err != nil {
			errs = append(errs, fmt.Errorf("%s: pattern %q が正規表現として正しくない", at, pre.Pattern))
		} else if !strings.HasPrefix(pre.Pattern, pre.Prefix+"-") {
			errs = append(errs, fmt.Errorf("%s: pattern %q が %q で始まっていない", at, pre.Pattern, pre.Prefix+"-"))
		}
		if len(pre.Files) == 0 {
			errs = append(errs, fmt.Errorf("%s: files がない", at))
		}
		switch {
		case pre.Find == FindTableFirstColumn:
		case pre.IsTableColumn() && pre.FindColumn() != "":
		default:
			errs = append(errs, fmt.Errorf("%s: find %q は %s か %s<列名> でない", at, pre.Find, FindTableFirstColumn, findTableColumn))
		}
		if c := pre.Children; c != nil {
			if c.Separator == "" {
				errs = append(errs, fmt.Errorf("%s: children.separator がない", at))
			}
			if len(c.Files) == 0 {
				errs = append(errs, fmt.Errorf("%s: children.files がない", at))
			}
			if c.ParentColumn == "" {
				errs = append(errs, fmt.Errorf("%s: children.parentColumn がない", at))
			}
			if c.NameColumn == "" {
				errs = append(errs, fmt.Errorf("%s: children.nameColumn がない", at))
			}
		}
	}
	return errs
}
