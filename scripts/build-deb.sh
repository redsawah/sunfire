#!/bin/sh
# Сборка .deb под amd64. Нужны go, ar, tar, gzip — dpkg не требуется.
# Использование: sh scripts/build-deb.sh [версия]
set -eu

VERSION="${1:-0.1.0}"
MAINTAINER="redsawah <117345533+redsawah@users.noreply.github.com>"
ARCHS="amd64"

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST="$ROOT/dist"
WORK="$DIST/.work"

rm -rf "$WORK"
mkdir -p "$DIST" "$WORK"

# Флаги tar у GNU и BSD разные — берём только те, что понимает местный tar.
TARFLAGS=""
: > "$WORK/probe"
tar_flag() {
	if tar $TARFLAGS "$@" -cf /dev/null -C "$WORK" probe >/dev/null 2>&1; then
		TARFLAGS="$TARFLAGS $*"
		return 0
	fi
	return 1
}
tar_flag --format=ustar || true
tar_flag --numeric-owner || true
# первый вариант — BSD tar, второй — GNU
tar_flag --uid=0 --gid=0 --uname=root --gname=root || tar_flag --owner=root --group=root || true
rm -f "$WORK/probe"

# S = не писать таблицу символов; без неё ar на macOS портит архив.
AROPT=rcS
if ! ( cd "$WORK" && : > s && ar $AROPT s.a s >/dev/null 2>&1 && ar t s.a 2>/dev/null | grep -qx s ); then
	AROPT=rc
fi
rm -f "$WORK/s" "$WORK/s.a"

md5_of() {
	if command -v md5sum >/dev/null 2>&1; then
		md5sum "$1" | cut -d' ' -f1
	elif command -v md5 >/dev/null 2>&1; then
		md5 -q "$1"
	else
		openssl md5 "$1" | sed 's/.*[= ]//'
	fi
}

# unit_gateway <файл>
unit_gateway() {
	cat > "$1" <<'EOF'
[Unit]
Description=sunfire, шлюз
After=network.target

[Service]
ExecStart=/usr/bin/sunfired -config /etc/sunfire/main.conf
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF
}

# unit_client <файл>
unit_client() {
	cat > "$1" <<'EOF'
[Unit]
Description=sunfire, клиент
After=network.target

[Service]
ExecStart=/usr/bin/sunfire
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF
}

# control <каталог> <пакет> <арх> <данные> <короткое описание> <вторая строка>
control() {
	size=$(du -sk "$4" | cut -f1)
	cat > "$1/control" <<EOF
Package: $2
Version: $VERSION
Section: net
Priority: optional
Architecture: $3
Maintainer: $MAINTAINER
Installed-Size: $size
Description: $5
 $6
EOF
}

# md5sums <каталог управления> <каталог данных>
md5sums() {
	: > "$1/md5sums"
	( cd "$2" && find . -type f | sed 's|^\./||' | sort ) | while read -r f; do
		printf '%s  %s\n' "$(md5_of "$2/$f")" "$f" >> "$1/md5sums"
	done
}

# pack <пакет> <арх> <каталог данных> <каталог управления>
pack() {
	deb="$DIST/$1_${VERSION}_$2.deb"
	tmp="$WORK/ar-$1-$2"
	rm -rf "$tmp"
	mkdir -p "$tmp"
	printf '2.0\n' > "$tmp/debian-binary"
	tar $TARFLAGS -czf "$tmp/control.tar.gz" -C "$4" .
	tar $TARFLAGS -czf "$tmp/data.tar.gz" -C "$3" .
	rm -f "$deb"
	( cd "$tmp" && ar $AROPT "$deb" debian-binary control.tar.gz data.tar.gz )
	printf '%s\n' "$deb"
}

for arch in $ARCHS; do
	bin="$WORK/bin/$arch"
	mkdir -p "$bin"
	for cmd in sunfire sunfired; do
		( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
			go build -trimpath \
				-ldflags "-s -w -X sunfire/internal/version.Number=$VERSION" \
				-o "$bin/$cmd" "./cmd/$cmd" )
	done

	# клиент
	d="$WORK/sunfire-$arch/data"
	c="$WORK/sunfire-$arch/control"
	mkdir -p "$d/usr/bin" "$d/lib/systemd/system" "$d/etc/sunfire" "$c"
	cp "$bin/sunfire" "$d/usr/bin/sunfire"
	unit_client "$d/lib/systemd/system/sunfire.service"
	cat > "$d/etc/sunfire/client.conf.example" <<'EOF'
{
  "socks": "127.0.0.1:1080",
  "transport": "auto",
  "streams": 1,
  "servers": [
    { "name": "node-a", "gw": "СЕРВЕР:9000", "token": "СЮДА ТОКЕН ОТ sunfired -token" }
  ]
}
EOF
	chmod 755 "$d/usr/bin/sunfire"
	chmod 644 "$d/lib/systemd/system/sunfire.service" "$d/etc/sunfire/client.conf.example"
	control "$c" sunfire "$arch" "$d" "клиент sunfire" "Локальный SOCKS5, соединения уходят на шлюз sunfired."
	md5sums "$c" "$d"
	pack sunfire "$arch" "$d" "$c"

	# шлюз
	d="$WORK/sunfired-$arch/data"
	c="$WORK/sunfired-$arch/control"
	mkdir -p "$d/usr/bin" "$d/lib/systemd/system" "$d/etc/sunfire" "$c"
	cp "$bin/sunfired" "$d/usr/bin/sunfired"
	unit_gateway "$d/lib/systemd/system/sunfired.service"
	cat > "$d/etc/sunfire/main.conf.example" <<'EOF'
{
  "listen": {
    "tcp": ":9000",
    "udp": ":9000"
  },
  "users": [
    { "name": "node-a", "token": "СЮДА ТОКЕН ОТ sunfired -token" }
  ]
}
EOF
	chmod 755 "$d/usr/bin/sunfired"
	chmod 644 "$d/lib/systemd/system/sunfired.service" "$d/etc/sunfire/main.conf.example"
	control "$c" sunfired "$arch" "$d" "шлюз sunfire" "Принимает соединения клиентов по TCP и UDP."
	md5sums "$c" "$d"
	pack sunfired "$arch" "$d" "$c"
done

rm -rf "$WORK"
