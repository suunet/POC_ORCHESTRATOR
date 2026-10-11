# エージェントの土台のイメージ。プロジェクトの .gateflow/agent.Dockerfile が、これを BASE として受け取り、言語の道具を足す。
# 検証用のコンテナ、gateflow の操作（clone、コミット、取り込み）のコンテナも、プロジェクトのイメージを使う。
FROM node:22-bookworm-slim

# git は clone とコミット、curl と ssh は完了の基準の確認（サンドボックスで外へ出られないこと）に使う
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl openssh-client \
 && rm -rf /var/lib/apt/lists/*

# Claude Code は版を固定して入れ、自動の更新と、必要のない通信（テレメトリ、エラーの報告）を止める
ARG CLAUDE_CODE_VERSION=2.1.220
RUN npm install --global @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# root でない利用者で動かす（sudo は入れない）。
# NOTE: 作業用のボリュームを初めて /vol にマウントすると、Docker はこのディレクトリの持ち主を写す。
# 特権を落としたコンテナは chown できないので、/vol を agent の持ち主で作っておく
RUN useradd --create-home --uid 10001 agent \
 && mkdir -p /vol /work /out /in /cache \
 && chown agent:agent /vol /work /out
USER agent
WORKDIR /work
