package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// 時間の上限で打ち切ったことを表す誤り。
var (
	ErrTimeout     = errors.New("実行の時間の上限に達した")
	ErrIdleTimeout = errors.New("出力のない時間の上限に達した")
)

// MountType は、マウントの種類。
type MountType string

const (
	Volume MountType = "volume"
	Bind   MountType = "bind"
)

// Mount は、コンテナへのマウント。Subpath はボリュームの一部だけをマウントするときに使う。
type Mount struct {
	Type     MountType
	Source   string
	Target   string
	Subpath  string
	ReadOnly bool
}

func (m Mount) arg() string {
	s := fmt.Sprintf("type=%s,source=%s,target=%s", m.Type, m.Source, m.Target)
	if m.Subpath != "" {
		s += ",volume-subpath=" + m.Subpath
	}
	if m.ReadOnly {
		s += ",readonly"
	}
	return s
}

// Spec は、コンテナの起動の仕様。コンテナに渡す環境変数は Env だけで、ホストの環境変数は引き継がない。
type Spec struct {
	Name    string // 時間の上限で消すときに使う。必須
	Image   string
	Network string
	User    string
	Workdir string
	Env     map[string]string
	Mounts  []Mount
	Cmd     []string

	Stdin  io.Reader // あれば標準入力をつなぐ
	TTY    bool      // 対話の端末（login など）
	Detach bool      // 起動して待たない。終わっても自動では消さない

	Timeout     time.Duration // 実行の全体の上限。0 なら上限なし
	IdleTimeout time.Duration // 出力のない時間の上限。0 なら上限なし
}

// Args は、docker run の引数を返す。
func (s Spec) Args() []string {
	args := []string{"run", "--rm"}
	if s.Detach {
		args[1] = "--detach"
	}
	args = append(args, "--name", s.Name)
	if s.Stdin != nil || s.TTY {
		args = append(args, "--interactive")
	}
	if s.TTY {
		args = append(args, "--tty")
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.Workdir != "" {
		args = append(args, "--workdir", s.Workdir)
	}
	// NOTE: setuid のプログラム（sudo など）で権限を上げられないようにし、Linux の特権もすべて落とす
	args = append(args, "--security-opt", "no-new-privileges", "--cap-drop", "ALL")
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	for _, m := range s.Mounts {
		args = append(args, "--mount", m.arg())
	}
	args = append(args, s.Image)
	return append(args, s.Cmd...)
}

// Docker は、docker のコマンドでコンテナを動かす。Command はテストで差し替える。
type Docker struct {
	Bin     string
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// NewDocker は、PATH の docker を使う Docker を返す。
func NewDocker() Docker {
	return Docker{Bin: "docker", Command: exec.CommandContext}
}

// Exec は、docker のコマンド（network、volume、build など）を実行して、出力を返す。
func (d Docker) Exec(ctx context.Context, args ...string) ([]byte, error) {
	out, err := d.Command(ctx, d.Bin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", args[0], err, out)
	}
	return out, nil
}

// Stream は、docker のコマンドを実行し、出力（標準出力と標準エラー）を終わるまで w へ逐次書く。
func (d Docker) Stream(ctx context.Context, w io.Writer, args ...string) error {
	cmd := d.Command(ctx, d.Bin, args...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", args[0], err)
	}
	return nil
}

// Run は、spec のコンテナを起動して終わるまで待ち、終了コードを返す。
// 時間の上限に達したら、コンテナを消して ErrTimeout か ErrIdleTimeout を返す。
func (d Docker) Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (int, error) {
	if spec.Name == "" {
		return 0, errors.New("コンテナの名前がない")
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if spec.Timeout > 0 {
		var stop context.CancelFunc
		runCtx, stop = context.WithTimeoutCause(runCtx, spec.Timeout, ErrTimeout)
		defer stop()
	}

	act := &activity{last: time.Now()}
	cmd := d.Command(runCtx, d.Bin, spec.Args()...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = act.wrap(stdout)
	cmd.Stderr = act.wrap(stderr)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("docker run を始められない: %w", err)
	}

	done := make(chan struct{})
	if spec.IdleTimeout > 0 {
		go act.watch(spec.IdleTimeout, done, func() { cancel(ErrIdleTimeout) })
	}
	err := cmd.Wait()
	close(done)

	if cause := context.Cause(runCtx); cause != nil {
		// NOTE: docker run のクライアントを止めてもコンテナは動き続けるので、名前で消す（時間の上限、呼び出し元の中止のどちらも）
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		d.Exec(rmCtx, "rm", "--force", spec.Name)
		return 0, cause
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, fmt.Errorf("docker run: %w", err)
	}
	return 0, nil
}

// activity は、最後に出力があった時刻を記録する。
type activity struct {
	mu   sync.Mutex
	last time.Time
}

func (a *activity) wrap(w io.Writer) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		a.mu.Lock()
		a.last = time.Now()
		a.mu.Unlock()
		return w.Write(p)
	})
}

func (a *activity) watch(limit time.Duration, done <-chan struct{}, onIdle func()) {
	tick := time.NewTicker(limit / 4)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			a.mu.Lock()
			idle := time.Since(a.last)
			a.mu.Unlock()
			if idle >= limit {
				onIdle()
				return
			}
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
