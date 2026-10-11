package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gateflow/internal/agent"
	"gateflow/internal/config"
	"gateflow/internal/container"
)

// Check は、サンドボックスで試す1つのこと。Expect が fail なら失敗すること、ok なら成功することを確かめる。
type Check struct {
	ID     string
	Group  string // push・post・module・env
	Desc   string
	Expect string
	Cmd    string
}

// CheckResult は、1つの確認の結果。Advice は、不合格のときの直し方の案内（自動では直さない）。
type CheckResult struct {
	Check
	ExitCode int
	Passed   bool
	Advice   string
}

// SandboxChecks は、完了の基準の 4.2〜4.4 で試すことの一覧。コマンドはエージェントのコンテナの /work で実行する。
func SandboxChecks() []Check {
	fail := func(id, group, desc, cmd string) Check { return Check{id, group, desc, "fail", cmd} }
	ok := func(id, group, desc, cmd string) Check { return Check{id, group, desc, "ok", cmd} }
	// NOTE: go get の確認は GOFLAGS を空にして、-mod=readonly ではなく取得先がないことで失敗するようにする
	goGet := `d=$(mktemp -d) && cd "$d" && go mod init probe && GOFLAGS= go get golang.org/x/text@v0.20.0`
	return []Check{
		fail("git-push-remote", "push", "clone から git push する（リモートがない）", `git push`),
		fail("git-push-github", "push", "GitHub へ git push する", `git push https://github.com/git/git.git HEAD:refs/heads/gateflow-probe`),
		fail("ssh-github", "push", "GitHub へ SSH で接続する", `ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no git@github.com true 2>&1 | grep -qE 'Permission denied|successfully authenticated'`),
		ok("no-credentials", "push", "git の認証情報とトークンの環境変数がない", `test -z "$(git config --get credential.helper)" && test ! -e ~/.git-credentials && test ! -e ~/.ssh && ! env | grep -iqE '^[^=]*(token|secret|password|api_key)[^=]*='`),
		fail("git-dir-write-hook", "push", ".git にフックを作る", `touch .git/hooks/pre-commit`),
		fail("git-dir-write-config", "push", ".git の設定を書き換える", `git config core.hooksPath /tmp`),
		fail("git-dir-commit", "push", "コミットする", `git commit --allow-empty -m probe`),
		fail("git-dir-write-attributes", "push", ".git/info/attributes を書き換える", `echo x > .git/info/attributes`),
		fail("post-not-allowed", "post", "許可リストにないホストへプロキシ経由で POST する", `curl -sS -f -m 10 -X POST -d x https://example.com/`),
		fail("post-direct-ip", "post", "プロキシを使わずに外部の IP へ POST する", `curl -sS -m 5 --noproxy '*' -X POST -d x https://1.1.1.1/`),
		fail("post-host-machine", "post", "ホストのマシンへ POST する", `curl -sS -m 5 --noproxy '*' -X POST -d x http://host.docker.internal/`),
		ok("allowed-host-reachable", "post", "許可リストのホスト（LLM の API）に届く", `test -n "$ALLOWED_HOST" && curl -sS -m 10 -o /dev/null "https://$ALLOWED_HOST/"`),
		fail("go-get-default", "module", "外部のモジュールを go get する（既定の設定）", goGet),
		fail("go-get-proxy", "module", "取得先を替えて go get する", `export GOPROXY=https://proxy.golang.org; `+goGet),
		fail("go-get-direct", "module", "取得先を使わずに直接 go get する", `export GOPROXY=direct; `+goGet),
		fail("curl-module-proxy", "module", "パッケージの取得先へ接続する", `curl -sS -m 10 -o /dev/null https://proxy.golang.org/`),
		fail("env-write", "env", ".env に書き込む", `echo X >> .env`),
		fail("env-remove", "env", ".env を消す", `rm .env`),
		fail("env-move", "env", ".env の名前を変える", `mv .env .env.moved`),
		fail("env-chmod", "env", ".env の権限を変える", `chmod 666 .env`),
		fail("sudo", "env", "sudo で権限を上げる", `sudo true`),
		// NOTE: 空のボリューム（またはその空のサブパス）をマウントすると、イメージのディレクトリの持ち主が写る。
		// root の持ち主になると、root でない利用者は書けない（M1 の /auth・/cache の誤り）
		ok("owner-mounts", "owner", "エージェントのマウント先（/vol・/work・/cache・/auth）の持ち主が root でない利用者",
			`for d in /vol /work /cache /auth; do [ "$(stat -c %U "$d")" = "$(id -un)" ] || exit 1; done`),
		ok("owner-volumes", "owner", "作業用のボリュームの根元とチケットごとのディレクトリの持ち主が root でない利用者",
			`[ "$(stat -c %U /check/vol)" = "$(id -un)" ] || exit 1; for d in /check/vol/*/ /check/vol/*/repo /check/vol/*/cache /check/vol/*/main; do [ -e "$d" ] || continue; [ "$(stat -c %U "$d")" = "$(id -un)" ] || exit 1; done`),
	}
}

// Sandbox は、エージェントと同じ構成のコンテナで、外へ出られないこと・書き換えられないことを確かめる。
type Sandbox struct {
	Root       string
	Project    *config.Project
	AgentImage string
	WorkVolume string // 作業用のボリューム（持ち主の検査で根元を見る）
	Agent      agent.Agent
	Workspace  Workspace
	Egress     Egress
	Runner     Runner

	ProxyLog []byte // 実行の後に、プロキシの判定の記録を入れる
}

const sandboxTicket = "doctor"

// Run は、確認をすべて実行して結果を返す。.env が変わっていないことはホストで確かめる。
func (s *Sandbox) Run(ctx context.Context) ([]CheckResult, error) {
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return nil, err
	}
	envFile := filepath.Join(root, s.Project.Agent.EnvTemplate)
	before, err := fileHash(envFile)
	if err != nil {
		return nil, fmt.Errorf(".env のひな形を読めない: %w", err)
	}
	if err := s.Workspace.Prepare(ctx, sandboxTicket, "gateflow-doctor", s.Project.Branch.Main, s.Project.Deps); err != nil {
		return nil, err
	}
	allow := append(slices.Clone(s.Agent.Hosts()), s.Project.Agent.Allow...)
	proxyURL, err := s.Egress.Start(ctx, allow)
	if err != nil {
		return nil, err
	}
	defer func() {
		s.ProxyLog, _ = s.Egress.Stop(context.WithoutCancel(ctx))
	}()
	outDir, err := os.MkdirTemp("", "gateflow-doctor-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(outDir)

	spec := agentSpec("gateflow-"+s.Project.Name+"-doctor-sandbox", s.AgentImage, s.Egress.Network(), proxyURL,
		s.Agent, s.Project, s.Workspace.AgentMounts(sandboxTicket), envFile, outDir,
		[]container.Mount{{Type: container.Volume, Source: s.WorkVolume, Target: "/check/vol", ReadOnly: true}})
	if len(allow) > 0 {
		spec.Env["ALLOWED_HOST"] = allow[0]
	}
	spec.Cmd = []string{"sh", "-c", probeScript(SandboxChecks())}
	spec.Timeout, spec.IdleTimeout = 10*time.Minute, 5*time.Minute
	var out bytes.Buffer
	if _, err := s.Runner.Run(ctx, spec, &out, &out); err != nil {
		return nil, fmt.Errorf("確認のコンテナを動かせない: %w", err)
	}
	codes := parseChecks(out.String())

	var results []CheckResult
	for _, c := range SandboxChecks() {
		code, ran := codes[c.ID]
		passed := ran && ((c.Expect == "fail" && code != 0) || (c.Expect == "ok" && code == 0))
		r := CheckResult{Check: c, ExitCode: code, Passed: passed}
		if !passed && c.Group == "owner" {
			r.Advice = fmt.Sprintf("root の持ち主のディレクトリがある。そのチケットのディレクトリを消して作り直す（docker run --rm --user agent -v %s:/vol %s sh -c 'rm -rf /vol/<チケット>'）か、ボリュームを消す（docker volume rm %s %s）。土台のイメージのマウント先の持ち主も確かめる",
				s.WorkVolume, s.AgentImage, s.WorkVolume, s.Agent.AuthMount().Source)
		}
		results = append(results, r)
	}
	after, err := fileHash(envFile)
	unchanged := err == nil && after == before
	results = append(results, CheckResult{
		Check:  Check{ID: "env-unchanged", Group: "env", Desc: "ホストの .env のひな形が変わっていない（実行の前後で比べる）", Expect: "ok"},
		Passed: unchanged,
	})
	return results, nil
}

// probeScript は、確認を順に実行し、「CHECK <ID> <終了コード>」の行を出すスクリプトを作る。
func probeScript(checks []Check) string {
	var b strings.Builder
	b.WriteString("cd /work\nrun() { timeout 60 sh -c \"$2\" >/dev/null 2>&1; echo \"CHECK $1 $?\"; }\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "run %s '%s'\n", c.ID, strings.ReplaceAll(c.Cmd, "'", `'\''`))
	}
	return b.String()
}

func parseChecks(out string) map[string]int {
	codes := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 && f[0] == "CHECK" {
			if code, err := strconv.Atoi(f[2]); err == nil {
				codes[f[1]] = code
			}
		}
	}
	return codes
}

func fileHash(path string) ([32]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}
