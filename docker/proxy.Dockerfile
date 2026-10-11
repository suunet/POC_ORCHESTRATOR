# 許可リストのプロキシのイメージ。ビルドの文脈はツール本体の bin/ で、BINARY に
# scripts/build.sh が作った Linux 用の実行ファイル（gateflow-linux-<CPU>）の名前を渡す。
#
# NOTE: FROM scratch で動く前提は次の2つ。プロキシが自分で外へ TLS を張る変更をしたら、この前提が崩れる
# （CA 証明書がないので、scratch のままでは接続に失敗する）
# - プロキシは CONNECT を中継するだけで、自分では TLS を張らない。そのため CA 証明書は要らない
# - 実行ファイルは CGO を使わずにビルドしているので、名前の解決は Go の純正のリゾルバが
#   Docker の /etc/resolv.conf を読んで行う
FROM scratch
ARG BINARY
COPY ${BINARY} /gateflow
USER 65534:65534
ENTRYPOINT ["/gateflow", "proxy"]
