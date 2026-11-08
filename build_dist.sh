#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────
# ChinaCPA 发布包构建：Windows exe + Linux 可直接运行的绿色包
#   用法：./build_dist.sh
#   产物：dist/ChinaCPA-windows-amd64.zip
#         dist/ChinaCPA-linux-amd64.tar.gz
#         dist/ChinaCPA-linux-arm64.tar.gz
#         dist/SHA256SUMS.txt
# ─────────────────────────────────────────────────────────────
set -euo pipefail
cd "$(dirname "$0")"

VER="${VER:-$(date +%Y%m%d-%H%M)}"
GOVER="$(go version | awk '{print $3}')"
LDFLAGS="-s -w"
PY="$(command -v python3 || command -v python || true)"
if [ -z "$PY" ]; then
  echo "[错误] 需要 python3 / python：用于打 zip 包与写入 tar 权限位"
  exit 1
fi

PKG_WIN="ChinaCPA-windows-amd64"
PKG_LNX_A="ChinaCPA-linux-amd64"
PKG_LNX_R="ChinaCPA-linux-arm64"

rm -rf build dist
mkdir -p build/win build/linux-amd64 build/linux-arm64 dist

build() { # $1=GOOS $2=GOARCH $3=out
  echo "  · $1/$2"
  GOOS="$1" GOARCH="$2" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$3" ./cmd/chinacpa
}

echo "[1/5] 交叉编译"
build windows amd64 build/win/chinacpa.exe
build linux amd64 build/linux-amd64/chinacpa
build linux arm64 build/linux-arm64/chinacpa

echo "[2/5] 组装 Windows 包"
cp config.json start.bat README.md build/win/
cp 测试指南.md build/win/TESTING.md
cat > build/win/BUILD-INFO.txt <<EOF
ChinaCPA 国产大模型反代网关
构建时间：$VER
Go 版本：$GOVER
目标平台：windows/amd64

启动：双击 start.bat（或命令行 chinacpa.exe -config config.json）
面板：http://127.0.0.1:8313/    密码见 config.json 的 panel_password
监听：默认仅本机（127.0.0.1:8313）；要让同网段设备访问，把 listen 改成 :8313
      并务必先改掉 panel_password
数据：./data（首次启动自动创建，包含账号与日志，请勿外传）

安全提示：
  config.json 的 panel_password 请改掉；listen 建议改成 127.0.0.1:8313 只监听本机；
  api_key 是网关调用密钥，默认值务必替换。
EOF

echo "[3/5] 组装 Linux 包"
for archdir in build/linux-amd64 build/linux-arm64; do
  cp config.json README.md "$archdir/"
  cp 测试指南.md "$archdir/TESTING.md"
  cp packaging/start.sh packaging/chinacpa.service "$archdir/"
  cat > "$archdir/BUILD-INFO.txt" <<EOF
ChinaCPA 国产大模型反代网关
构建时间：$VER
Go 版本：$GOVER
目标平台：linux/$(basename "$archdir" | sed 's/linux-//')

启动：./start.sh        前台运行
      ./start.sh -d     后台运行（日志 chinacpa.log）
      systemctl 托管见 chinacpa.service（改好路径后放到 /etc/systemd/system/）
面板：本机 http://127.0.0.1:8313/    局域网 http://<服务器IP>:8313/
监听：0.0.0.0:8313（所有网卡）。只想本机访问时改成 127.0.0.1:8313
      并务必先改掉 panel_password
数据：./data（首次启动自动创建，包含账号与日志，请勿外传）

依赖：无。纯静态二进制，glibc/musl 均可运行；豆包浏览器签到需要本机装 Chrome/Chromium。

安全提示：
  config.json 的 panel_password / api_key 请改掉；暴露到局域网等于把账号 Cookie 交出去；
  只要本机访问，把 listen 改成 127.0.0.1:8313。
EOF
  chmod 755 "$archdir/chinacpa" "$archdir/start.sh"
done

echo "[4/5] 打包"
# Windows 默认只听本机；Linux 听 0.0.0.0，方便同网段访问。口令仍是默认值，务必先改。
"$PY" - <<'PY'
import json, os
listen_by_dir = {
    "build/win": "127.0.0.1:8313",
    "build/linux-amd64": "0.0.0.0:8313",
    "build/linux-arm64": "0.0.0.0:8313",
}
for d, listen in listen_by_dir.items():
    p = os.path.join(d, "config.json")
    cfg = json.load(open(p, encoding="utf-8"))
    cfg["listen"] = listen
    with open(p, "w", encoding="utf-8", newline="\n") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print("  · %s listen=%s" % (d, listen))
PY

ABS_DIST="$(cd dist && pwd)"
# 注意：Windows 上 chmod 不落盘，tar 的 --mode 也不生效，
# 因此用 python tarfile 显式写 mode，保证解压后 chinacpa 与 start.sh 可执行。
"$PY" - <<'PY'
import os, tarfile

EXEC = {"chinacpa", "start.sh"}
FILES = ["chinacpa", "start.sh", "config.json", "README.md",
         "TESTING.md", "chinacpa.service", "BUILD-INFO.txt"]

for arch in ("linux-amd64", "linux-arm64"):
    src = os.path.join("build", arch)
    out = os.path.join("dist", "ChinaCPA-%s.tar.gz" % arch)
    with tarfile.open(out, "w:gz") as t:
        for name in FILES:
            p = os.path.join(src, name)
            info = t.gettarinfo(p, arcname="./" + name)
            info.mode = 0o755 if name in EXEC else 0o644
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            with open(p, "rb") as f:
                t.addfile(info, f)
    print("  · dist/ChinaCPA-%s.tar.gz" % arch)
PY

# zip 用 python 生成，Windows 资源管理器可直接双击解压
"$PY" - <<'PY'
import os, zipfile
src = os.path.join("build", "win")
out = os.path.join("dist", "ChinaCPA-windows-amd64.zip")
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
    for name in sorted(os.listdir(src)):
        p = os.path.join(src, name)
        zi = zipfile.ZipInfo.from_file(p, name)
        mode = 0o755 if name.lower().endswith((".exe", ".bat")) else 0o644
        zi.external_attr = (0o100000 | mode) << 16
        zi.compress_type = zipfile.ZIP_DEFLATED
        with open(p, "rb") as f:
            z.writestr(zi, f.read())
print("  · dist/ChinaCPA-windows-amd64.zip")
PY

echo "[5/5] 校验和"
( cd dist && sha256sum ./*.zip ./*.tar.gz > SHA256SUMS.txt && cat SHA256SUMS.txt )

echo
echo "完成，产物在 dist/："
ls -lh dist/ | awk 'NR>1 {printf "  %-38s %s\n", $9, $5}'
