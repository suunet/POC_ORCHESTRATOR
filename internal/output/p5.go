// Package output は、AIの各stepの出力（/out/result.json）の構造体と検証を持つ。
//
// AIに渡す出力形式の説明は、構造体のタグ（json・desc・enum）から作る。
package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ResultPath は、エージェントのコンテナの中で出力を書く場所。
const ResultPath = "/out/result.json"

// P5 の outcome の選択肢。
const (
	OutcomeDone       = "done"
	OutcomeTestDefect = "test_defect"
	OutcomeBlocked    = "blocked"
)

// blocked の理由の選択肢。
const (
	BlockScope      = "scope"      // 指示書の範囲の外の変更が必要
	BlockDependency = "dependency" // 依存の追加が必要
)

// Question は、人に確かめたいこと。空でなければ質疑に登録する。
type Question struct {
	Text    string   `json:"text" desc:"質問の本文。判断の材料を含める"`
	Options []string `json:"options,omitempty" desc:"選択肢（任意）"`
}

// TestMapping は、書いたテストと受入条件の対応。
type TestMapping struct {
	Test       string   `json:"test" desc:"テストの名前"`
	File       string   `json:"file" desc:"テストのファイル（リポジトリのルートからの相対パス）"`
	Acceptance []string `json:"acceptance,omitempty" desc:"対応する受入条件のID（例：UC-001-AC-01）。受入条件を持たない基盤のテストは空でよい"`
}

// P5Result は、M1 の step p5（テストを先に書いて実装する）の出力。
type P5Result struct {
	Outcome       string        `json:"outcome" desc:"結果" enum:"done,test_defect,blocked"`
	Summary       string        `json:"summary" desc:"行ったことの要約"`
	Questions     []Question    `json:"questions,omitempty" desc:"人に確かめたいこと（推測で埋めない）"`
	Notes         []string      `json:"notes,omitempty" desc:"補足（気づいたが扱わなかったことなど）"`
	Tests         []TestMapping `json:"tests,omitempty" desc:"書いたテストと受入条件の対応。outcome が done なら1件以上"`
	ChangedSheets []string      `json:"changedSheets,omitempty" desc:"更新した文書（指示書の§4.4に挙がったものだけ）"`
	BlockReason   string        `json:"blockReason,omitempty" desc:"outcome が blocked のときの理由。scope は指示書の範囲の外の変更が必要、dependency は依存の追加が必要" enum:"scope,dependency"`
}

// ParseP5 は、P5 の出力を読み込んで検査する。誤りはまとめて返す。
func ParseP5(data []byte) (*P5Result, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r P5Result
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("出力が JSON として正しくない: %w", err)
	}
	if errs := r.validate(); len(errs) > 0 {
		return nil, fmt.Errorf("出力の内容が正しくない:\n%w", errors.Join(errs...))
	}
	return &r, nil
}

func (r *P5Result) validate() []error {
	var errs []error
	switch r.Outcome {
	case "":
		errs = append(errs, errors.New("outcome がない"))
	case OutcomeDone, OutcomeTestDefect, OutcomeBlocked:
	default:
		errs = append(errs, fmt.Errorf("outcome %q は done・test_defect・blocked のどれでもない", r.Outcome))
	}
	if r.Summary == "" {
		errs = append(errs, errors.New("summary がない"))
	}
	if r.Outcome == OutcomeDone && len(r.Tests) == 0 {
		errs = append(errs, errors.New("outcome が done なのに tests がない"))
	}
	for i, tm := range r.Tests {
		if tm.Test == "" {
			errs = append(errs, fmt.Errorf("tests[%d].test がない", i))
		}
		if tm.File == "" {
			errs = append(errs, fmt.Errorf("tests[%d].file がない", i))
		}
	}
	for i, q := range r.Questions {
		if q.Text == "" {
			errs = append(errs, fmt.Errorf("questions[%d].text がない", i))
		}
	}
	switch {
	case r.Outcome == OutcomeBlocked && r.BlockReason == "":
		errs = append(errs, errors.New("outcome が blocked なのに blockReason がない"))
	case r.Outcome != OutcomeBlocked && r.BlockReason != "":
		errs = append(errs, fmt.Errorf("outcome が %s なのに blockReason がある", r.Outcome))
	case r.BlockReason != "" && !slices.Contains([]string{BlockScope, BlockDependency}, r.BlockReason):
		errs = append(errs, fmt.Errorf("blockReason %q は scope・dependency のどれでもない", r.BlockReason))
	}
	return errs
}

// DescribeP5 は、AIに渡す P5 の出力形式の説明を返す。
func DescribeP5() string {
	return describe(ResultPath, P5Result{})
}
