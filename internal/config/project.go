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
const FormatVersion = 0

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
	IDs             *IDs     `json:"ids"`
	Policies        []Policy `json:"policies"`
}

// IDs は、ID整合チェックの設定。
type IDs struct {
	// Scan は、参照を探すファイルまたはディレクトリ（ルートからの相対パス）。定義のファイルは常に含む。
	Scan []string `json:"scan,omitempty"`
	// DeprecatedMarker は、定義の行のセルがこの文字列で始まるとき、その定義を廃止とみなす印。
	DeprecatedMarker string   `json:"deprecatedMarker,omitempty"`
	Prefixes         []Prefix `json:"prefixes"`
}

// Prefix は、1つのIDのプレフィックスの定義の見つけ方。
type Prefix struct {
	Prefix     string    `json:"prefix"`
	Pattern    string    `json:"pattern"`
	Files      []string  `json:"files"`
	Find       string    `json:"find"`
	NameColumn string    `json:"nameColumn,omitempty"`
	Children   *Children `json:"children,omitempty"`
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

var validRoles = map[string]bool{"dev": true, "bj": true}

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
				errs = append(errs, fmt.Errorf("policies[%d]: roles の %q は dev か bj でない", i, r))
			}
		}
	}
	return errs
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
