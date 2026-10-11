// Package agent は、AI のエージェント（CLI の非対話実行）のインターフェースを持つ。
//
// AI サービスに固有の値（コマンド、認証の置き場所、許可リストのホスト）は、各アダプタ（claude/ など）が持つ。
package agent

import "gateflow/internal/container"

// Request は、エージェントへの1回の依頼。
type Request struct {
	Step   string // step の名前（例：p5）
	System string // システムプロンプトに足すもの（Persona）
	Prompt string // ユーザーメッセージ（Knowledge・Instruction・Output Contract・Policy）
	Model  string // 空なら指定しない
}

// Agent は、コンテナの中で動かすエージェント。
type Agent interface {
	Name() string
	// Command は、コンテナで実行するコマンドと、標準入力に渡す内容を返す。
	Command(req Request) (cmd []string, stdin string)
	// Hosts は、許可リストに足すホスト（host:port）。LLM の API と認証に必要なものだけ。
	Hosts() []string
	// AuthMount は、認証の情報を置くボリュームのマウント（git の認証情報は入れない）。
	AuthMount() container.Mount
	// Env は、エージェントのコンテナに渡す環境変数。
	Env() map[string]string
	// LoginCommand は、gateflow login で対話の端末で動かすコマンド。
	LoginCommand() []string
}
