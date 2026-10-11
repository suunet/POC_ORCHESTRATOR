package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PersonalFile は、ホームディレクトリから見た個人の設定の場所。
const PersonalFile = ".gateflow/config.json"

// 使える AI サービス。Cursor は M6 で足す。
const AgentClaude = "claude"

// Personal は、個人の設定（~/.gateflow/config.json）の中身。
type Personal struct {
	// Agent は、使う AI サービス。既定値は設けない（許可のない人が意図せず使うことを防ぐ）。
	Agent string `json:"agent"`
	// Model は、AI サービスに渡すモデルの名前。空なら指定しない。
	Model string `json:"model,omitempty"`
}

// LoadPersonal は、home/.gateflow/config.json を読み込んで検査する。
func LoadPersonal(home string) (*Personal, error) {
	path := filepath.Join(home, PersonalFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("個人の設定 %s を読めない（使う AI サービスを agent に書く）: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Personal
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("個人の設定 %s の形式が正しくない: %w", path, err)
	}
	switch p.Agent {
	case AgentClaude:
	case "":
		return nil, errors.New("個人の設定に agent（使う AI サービス）がない")
	default:
		return nil, fmt.Errorf("個人の設定の agent %q は使えない（使えるのは %s）", p.Agent, AgentClaude)
	}
	return &p, nil
}
