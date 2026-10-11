package engine

import (
	"gateflow/internal/agent"
	"gateflow/internal/config"
	"gateflow/internal/container"
	"gateflow/internal/workspace"
)

// agentSpec は、エージェントのコンテナの仕様を作る。P5 とサンドボックスの確認で同じものを使う。
//
// 内部ネットワークに置き、外へはプロキシ（proxyURL）だけを通す。clone の .git、依存のキャッシュ、.env は読み取り専用。
func agentSpec(name, image, network, proxyURL string, ag agent.Agent, project *config.Project,
	wsMounts []container.Mount, envFile, outDir string, extra []container.Mount) container.Spec {
	mounts := append([]container.Mount{}, wsMounts...)
	mounts = append(mounts,
		ag.AuthMount(),
		container.Mount{Type: container.Bind, Source: envFile, Target: "/work/.env", ReadOnly: true},
		container.Mount{Type: container.Bind, Source: outDir, Target: "/out"},
	)
	mounts = append(mounts, extra...)

	env := ag.Env()
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		env[k] = proxyURL
	}
	env["NO_PROXY"], env["no_proxy"] = "localhost,127.0.0.1", "localhost,127.0.0.1"
	for k, v := range offlineEnv(project) {
		env[k] = v
	}
	return container.Spec{
		Name:    name,
		Image:   image,
		Network: network,
		User:    workspace.User,
		Workdir: "/work",
		Env:     env,
		Mounts:  mounts,
	}
}

// offlineEnv は、外のモジュールを取得させない環境変数（エージェントと検証用のコンテナ）。
func offlineEnv(project *config.Project) map[string]string {
	env := map[string]string{"GOPROXY": "off", "GOFLAGS": "-mod=readonly", "HOME": "/home/agent"}
	if project.Deps != nil {
		env[project.Deps.CacheEnv] = "/cache"
	}
	return env
}
