#!/bin/sh
set -eu

DEPLOY_USER=sendandsafe-deploy
APP_DIR=/opt/send-and-safe
COMPOSE_SOURCE=${COMPOSE_SOURCE:-"$(dirname "$0")/compose.production.yaml"}
: "${DEPLOY_PUBLIC_KEY:?DEPLOY_PUBLIC_KEY is required}"

if ! id "$DEPLOY_USER" >/dev/null 2>&1; then
    useradd --create-home --shell /bin/bash "$DEPLOY_USER"
fi
usermod -aG docker "$DEPLOY_USER"

install -d -m 0700 -o "$DEPLOY_USER" -g "$DEPLOY_USER" "/home/$DEPLOY_USER/.ssh"
AUTHORIZED_KEYS="/home/$DEPLOY_USER/.ssh/authorized_keys"
touch "$AUTHORIZED_KEYS"
chown "$DEPLOY_USER:$DEPLOY_USER" "$AUTHORIZED_KEYS"
chmod 0600 "$AUTHORIZED_KEYS"
grep -qxF "$DEPLOY_PUBLIC_KEY" "$AUTHORIZED_KEYS" || printf '%s\n' "$DEPLOY_PUBLIC_KEY" >> "$AUTHORIZED_KEYS"

install -d -m 0755 "$APP_DIR"
install -m 0644 "$COMPOSE_SOURCE" "$APP_DIR/compose.yaml"
touch "$APP_DIR/.env"
chown "$DEPLOY_USER:$DEPLOY_USER" "$APP_DIR/.env"
chmod 0600 "$APP_DIR/.env"

cat > /usr/local/sbin/prepare-send-and-safe-container <<'SCRIPT'
#!/bin/sh
set -eu
install -m 0644 /home/sendandsafe-deploy/compose.production.yaml /opt/send-and-safe/compose.yaml
systemctl disable --now send-and-safe.service 2>/dev/null || true
SCRIPT
chmod 0755 /usr/local/sbin/prepare-send-and-safe-container

cat > /etc/sudoers.d/sendandsafe-deploy <<'SUDOERS'
sendandsafe-deploy ALL=(root) NOPASSWD: /usr/local/sbin/prepare-send-and-safe-container
SUDOERS
chmod 0440 /etc/sudoers.d/sendandsafe-deploy

echo "Docker deployment user configured."
