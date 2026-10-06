#!/usr/bin/env bash
# Linux（systemd）一键安装脚本。
#
# 做的事：
#   1. 创建专用系统用户与数据目录
#   2. 安装二进制到 /opt/wb-gateway，数据目录设为 /var/lib/wb-gateway
#   3. 安装 systemd 单元并启用开机自启
#
# 用法（在解压出来的发行包里执行，需 root）：
#   sudo ./install-systemd.sh
#
# 幂等：重复执行只会覆盖二进制并重启服务，不会动已有的账号池与配置。
set -euo pipefail

BIN_DIR=/opt/wb-gateway
DATA_DIR=/var/lib/wb-gateway
SERVICE=/etc/systemd/system/wb-gateway.service
UNIT_NAME=wb-gateway.service

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 root 执行：sudo $0" >&2
  exit 1
fi

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ ! -f "$SRC_DIR/workbuddy-gateway" ]; then
  # 发行包内是平台化命名的二进制，这里兜底找一次
  CAND="$(find "$SRC_DIR" -maxdepth 1 -name 'workbuddy-gateway*' -type f | head -1 || true)"
  if [ -z "$CAND" ]; then
    echo "未在当前目录找到 workbuddy-gateway 二进制" >&2
    exit 1
  fi
  cp "$CAND" "$SRC_DIR/workbuddy-gateway"
fi

echo "==> 创建用户与目录"
if ! id -u wb-gateway >/dev/null 2>&1; then
  useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin wb-gateway
fi
install -d -o wb-gateway -g wb-gateway -m 0750 "$DATA_DIR"
install -d -o root -g root -m 0755 "$BIN_DIR"

echo "==> 安装二进制"
install -o root -g root -m 0755 "$SRC_DIR/workbuddy-gateway" "$BIN_DIR/workbuddy-gateway"

# 本机代理是可选件：装了它面板上才会出现「本机客户端」页。
# 服务端部署通常不需要，所以存在才装，不强制报错。
if [ -f "$SRC_DIR/wb-local-agent" ]; then
  install -o root -g root -m 0755 "$SRC_DIR/wb-local-agent" "$BIN_DIR/wb-local-agent"
fi

if [ -f "$SRC_DIR/config.example.json" ] && [ ! -f "$DATA_DIR/config.example.json" ]; then
  install -o wb-gateway -g wb-gateway -m 0640 "$SRC_DIR/config.example.json" "$DATA_DIR/config.example.json"
fi

echo "==> 安装 systemd 单元"
if [ -f "$SRC_DIR/wb-gateway.service" ]; then
  install -o root -g root -m 0644 "$SRC_DIR/wb-gateway.service" "$SERVICE"
else
  echo "!! 发行包里没有 wb-gateway.service，跳过" >&2
fi

systemctl daemon-reload
systemctl enable --now "$UNIT_NAME"
systemctl --no-pager --lines=0 status "$UNIT_NAME" || true

cat <<EOF

==> 安装完成

 查看日志   journalctl -u $UNIT_NAME -f
 重启       systemctl restart $UNIT_NAME
 停止       systemctl stop $UNIT_NAME
 面板       http://127.0.0.1:8317/panel/

把凭据文件放进 $DATA_DIR（或写进 config.json 的 auth 段），面板上即可看到账号。
注意：网关启动时会先同步拉一次模型目录再监听端口，因此最初几十秒内
/healthz 会连接拒绝，这是预期行为。
EOF
