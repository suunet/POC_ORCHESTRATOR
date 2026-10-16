// Package viewpoints は、観点表（プロジェクトの .gateflow/viewpoints.csv）を読む（M3）。
// ツールには列の定義（builtins/viewpoints/columns.json）だけを置き、中身はプロジェクトが持つ。
package viewpoints

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// ProjectFile は、プロジェクトのルートから見た観点表の場所。
const ProjectFile = ".gateflow/viewpoints.csv"

// ColumnsFile は、組み込みの層の中の列の定義の場所。
const ColumnsFile = "viewpoints/columns.json"

// Columns は、観点表の列の定義。Required の列は必須で、ほかの列はそのまま P3 に渡す。
type Columns struct {
	IDColumn     string   `json:"idColumn"`
	Required     []string `json:"required"`
	MethodColumn string   `json:"methodColumn"`
	Methods      []string `json:"methods"`
}

// LoadColumns は、組み込みの層から列の定義を読む。
func LoadColumns(fsys fs.FS) (Columns, error) {
	data, err := fs.ReadFile(fsys, ColumnsFile)
	if err != nil {
		return Columns{}, err
	}
	var c Columns
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Columns{}, fmt.Errorf("%s を読めない: %w", ColumnsFile, err)
	}
	if c.IDColumn == "" || !slices.Contains(c.Required, c.IDColumn) || (c.MethodColumn != "" && !slices.Contains(c.Required, c.MethodColumn)) {
		return Columns{}, fmt.Errorf("%s：idColumn と methodColumn は required の列に要る", ColumnsFile)
	}
	return c, nil
}

// Table は、読んだ観点表。
type Table struct {
	ids  map[string]bool
	Rows int
}

// Has は、id が観点表にあるかを返す。
func (t *Table) Has(id string) bool { return t.ids[id] }

// Parse は、観点表の CSV を列の定義に照らして読む。必須の列がない、ID が空か重なる、確認の方法が定義にない値なら誤り。
func Parse(data []byte, c Columns) (*Table, error) {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	r := csv.NewReader(bytes.NewReader(data))
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("観点表（%s）を CSV として読めない: %w", ProjectFile, err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("観点表（%s）に見出しの行がない", ProjectFile)
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[strings.TrimSpace(h)] = i
	}
	var errs []error
	for _, name := range c.Required {
		if _, ok := col[name]; !ok {
			errs = append(errs, fmt.Errorf("必須の列 %q がない", name))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("観点表（%s）が列の定義に合わない:\n%w", ProjectFile, errors.Join(errs...))
	}
	t := &Table{ids: map[string]bool{}}
	for n, rec := range records[1:] {
		line := n + 2
		id := strings.TrimSpace(rec[col[c.IDColumn]])
		switch {
		case id == "":
			errs = append(errs, fmt.Errorf("%d 行目：%s が空", line, c.IDColumn))
		case t.ids[id]:
			errs = append(errs, fmt.Errorf("%d 行目：%s %q が重なっている", line, c.IDColumn, id))
		}
		t.ids[id] = true
		if c.MethodColumn != "" {
			if m := strings.TrimSpace(rec[col[c.MethodColumn]]); !slices.Contains(c.Methods, m) {
				errs = append(errs, fmt.Errorf("%d 行目：%s %q は %s のどれでもない", line, c.MethodColumn, m, strings.Join(c.Methods, "・")))
			}
		}
		t.Rows++
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("観点表（%s）が列の定義に合わない:\n%w", ProjectFile, errors.Join(errs...))
	}
	return t, nil
}
