#!/bin/sh
# DropBridge installer for ZimaOS / CasaOS (and plain Linux).
# Pulls the prebuilt multi-arch image from GHCR, runs the app, and — on ZimaOS —
# registers a FIXED-URL dashboard tile (a LinkApp) that opens this node's own
# tailnet address, so the tile works no matter how you reach the dashboard.
#
#   Run on the host with docker access (root / sudo):   sudo sh install.sh
#
# Why a LinkApp and not a normal CasaOS store tile: a store tile builds its launch
# URL from the ACCESS host, so opening the dashboard over the LAN IP makes the tile
# open the LAN origin → the control-plane guard 403s → the UI shows demo mode. A
# LinkApp URL is fixed. Trade-off: the container is not app-managed (no health
# tile); it stays boot-safe via `restart: unless-stopped`.
set -eu

IMAGE="ghcr.io/chicohaager/dropbridge:0.2.1"
DEPLOY=/DATA/AppData/dropbridge/deploy      # persistent standalone deploy dir
STATE=/DATA/AppData/dropbridge/state        # peers/shares/settings
INCOMING=/DATA/dropbridge/incoming          # fallback received-files path
APPS=/var/lib/casaos/apps/dropbridge
LINK=/var/lib/casaos/1/link.json
ORDER=/var/lib/casaos/1/app_order.json
UID_APP=1000                                # container runs non-root as uid 1000
DC="env DOCKER_CONFIG=${DOCKER_CONFIG:-/DATA/.docker} docker"
PORT=8787
# DropBridge tile icon (self-contained data-URI — no network fetch, no tailnet dep)
ICON_URI='data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA1MTIgNTEyIiB3aWR0aD0iNTEyIiBoZWlnaHQ9IjUxMiI+CiAgPGRlZnM+CiAgICA8bGluZWFyR3JhZGllbnQgaWQ9ImciIHgxPSIwIiB5MT0iMCIgeDI9IjEiIHkyPSIxIj4KICAgICAgPHN0b3Agb2Zmc2V0PSIwIiBzdG9wLWNvbG9yPSIjMmJjNGM5Ii8+CiAgICAgIDxzdG9wIG9mZnNldD0iMSIgc3RvcC1jb2xvcj0iIzFhOGY5NyIvPgogICAgPC9saW5lYXJHcmFkaWVudD4KICA8L2RlZnM+CiAgPHJlY3Qgd2lkdGg9IjUxMiIgaGVpZ2h0PSI1MTIiIHJ4PSIxMTIiIGZpbGw9InVybCgjZykiLz4KICA8ZyBmaWxsPSJub25lIiBzdHJva2U9IiMwNDE5MWEiIHN0cm9rZS13aWR0aD0iMzAiIHN0cm9rZS1saW5lY2FwPSJyb3VuZCIgc3Ryb2tlLWxpbmVqb2luPSJyb3VuZCIgb3BhY2l0eT0iMC45MiI+CiAgICA8cGF0aCBkPSJNMTUwIDE5NiBoMjEyIi8+CiAgICA8cGF0aCBkPSJNMzAwIDE0NiBsNjIgNTAgLTYyIDUwIi8+CiAgICA8cGF0aCBkPSJNMzYyIDMxNiBoLTIxMiIvPgogICAgPHBhdGggZD0iTTIxMiAyNjYgbC02MiA1MCA2MiA1MCIvPgogIDwvZz4KPC9zdmc+Cg=='

echo "-> DropBridge installer ($IMAGE)"

# 1) shared bearer token as a file secret (0400, owned by container app user uid 1000)
mkdir -p "$DEPLOY"
if [ ! -f "$DEPLOY/secret.token" ]; then
  # umask scoped to a subshell: it must not leak into the state/incoming mkdirs below
  ( umask 077; LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 48 > "$DEPLOY/secret.token" )
  chmod 0400 "$DEPLOY/secret.token"
  echo "-> generated secret.token (48 chars). For a 2-node mesh, copy this SAME"
  echo "   file to $DEPLOY/secret.token on the other box."
fi
# The container user (uid 1000) must be able to READ the 0400 token — a silent
# chown failure would start the app with ingest disabled ("no token").
chown "$UID_APP:$UID_APP" "$DEPLOY/secret.token" || echo "!! WARN: chown secret.token failed — the app (uid $UID_APP) may not read it."

# 2) node config (.env). Edit DROPBRIDGE_NODE / PEER_NAME / PEER_URL afterwards.
if [ ! -f "$DEPLOY/.env" ]; then
  cat > "$DEPLOY/.env" <<'ENV'
# This node's identity
DROPBRIDGE_NODE=my-node
# The peer it sends to (leave PEER_URL empty for a single-node install)
DROPBRIDGE_PEER_NAME=peer-node
DROPBRIDGE_PEER_URL=
# Optional: public share links need a matching `tailscale funnel`
# DROPBRIDGE_FUNNEL_BASE=https://<node>.<your-tailnet>.ts.net:10000
ENV
  chmod 600 "$DEPLOY/.env"
  echo "-> created $DEPLOY/.env — edit DROPBRIDGE_NODE / PEER_NAME / PEER_URL."
fi

# 3) prepare native data disks for the storage picker. The non-root app (uid 1000)
#    can't create dirs on root-owned /media disk roots, so pre-make+chown a
#    "DropBridge" dir on each NATIVE disk. Foreign FS (exFAT/NTFS/vfat) and eMMC are
#    skipped — they can't hold Unix ownership / shouldn't be a target.
if [ -d /media ]; then
  echo "-> preparing data disks under /media ..."
  while IFS=' ' read -r src mp fstype _rest; do
    case "$mp" in
      /media/*/*) continue ;;
      /media/*)   ;;
      *)          continue ;;
    esac
    case "$fstype" in ext4|ext3|ext2|xfs|btrfs) ;; *) continue ;; esac
    case "$src" in *mmcblk*) continue ;; esac
    if mkdir -p "$mp/DropBridge" 2>/dev/null && chown "$UID_APP:$UID_APP" "$mp/DropBridge" 2>/dev/null; then
      echo "   prepared $mp/DropBridge"
    fi
  done <<EOF
$(awk '{print $1, $2, $3}' /proc/mounts)
EOF
fi

# 4) fixed state dir + fallback incoming — both persistent, both writable by uid 1000
#    (an unwritable state dir = peers/shares/storage choice silently not persisted)
mkdir -p "$STATE" "$INCOMING"
chown -R "$UID_APP:$UID_APP" "$STATE" "$INCOMING" || echo "!! WARN: chown $STATE / $INCOMING failed — the app may not be able to write there."

# 4b) preflight: the tailscaled LocalAPI socket must exist ON THE HOST. DropBridge
#     needs it for tailnet identity + presence. If Tailscale runs as the ZimaOS
#     App-Store *container*, its socket lives only inside that container — expose it
#     to the host (bind /var/run/tailscale -> /run/tailscale on the Tailscale app).
if [ ! -S /var/run/tailscale/tailscaled.sock ]; then
  echo ""
  echo "!! WARNING: no tailscaled socket at /var/run/tailscale/tailscaled.sock —"
  echo "!! DropBridge needs it for tailnet identity + presence. If Tailscale is the"
  echo "!! ZimaOS App-Store app (a container), add a bind mount to it:"
  echo "!!     /var/run/tailscale   ->   /run/tailscale"
  echo "!! then recreate that app and re-run this installer. Continuing (expect demo"
  echo "!! mode until the host socket is present)."
  echo ""
fi

# 5) write the tile-free compose (standalone; NO x-casaos/labels so app-management
#    doesn't render an access-host store tile) and pull the image.
cat > "$DEPLOY/docker-compose.yml" <<COMPOSE
name: dropbridge
services:
  dropbridge:
    image: $IMAGE
    container_name: dropbridge
    restart: unless-stopped
    env_file: .env
    environment:
      - DROPBRIDGE_INCOMING=/data/incoming
      - DROPBRIDGE_TOKEN_FILE=/run/secrets/dropbridge_token
      - DROPBRIDGE_STATE=/state
    secrets:
      - dropbridge_token
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "-", "http://127.0.0.1:$PORT/api/config"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 5s
    network_mode: host
    volumes:
      - type: bind
        source: $INCOMING
        target: /data/incoming
      - type: bind
        source: /var/run/tailscale
        target: /var/run/tailscale
        read_only: true
      - type: bind
        source: /media
        target: /media
      - type: bind
        source: $STATE
        target: /state
secrets:
  dropbridge_token:
    file: ./secret.token
COMPOSE
chmod 600 "$DEPLOY/docker-compose.yml"

echo "-> pulling image ..."
( cd "$DEPLOY" && $DC compose pull ) || echo "   (pull failed — is the GHCR image public? continuing if a local image exists)"

# 6) start standalone + register the fixed-URL dashboard tile (ZimaOS only).
if [ -e "$APPS/docker-compose.yml" ]; then
  echo "-> removing earlier compose-store tile registration ($APPS) ..."
  rm -rf "$APPS.removed-by-linkapp" 2>/dev/null || true
  mv "$APPS" "$APPS.removed-by-linkapp" 2>/dev/null || true
fi

( cd "$DEPLOY" && $DC compose up -d --force-recreate )
# not app-managed → guarantee boot autostart via the docker restart policy
$DC update --restart unless-stopped dropbridge >/dev/null 2>&1 || true

if [ -d /var/lib/casaos ]; then
  # this node's FIXED tailnet tile URL: prefer the MagicDNS name, else tailnet IPv4.
  TSURL=""
  if command -v tailscale >/dev/null 2>&1; then
    TSNAME=$(tailscale status --json 2>/dev/null | jq -r '.Self.DNSName // empty' 2>/dev/null | sed 's/\.$//')
    if [ -n "$TSNAME" ]; then
      TSURL="http://$TSNAME:$PORT/"
    else
      TSIP=$(tailscale ip -4 2>/dev/null | head -1)
      [ -n "$TSIP" ] && TSURL="http://$TSIP:$PORT/"
    fi
  fi

  if [ -n "$TSURL" ] && [ -f "$LINK" ] && command -v jq >/dev/null 2>&1; then
    tmp=$(mktemp) || tmp=/tmp/dropbridge-link.$$
    if jq --arg url "$TSURL" --arg icon "$ICON_URI" \
         'map(select(.name!="DropBridge")) + [{hostname:$url,name:"DropBridge",icon:$icon,app_type:"LinkApp",status:"running"}]' \
         "$LINK" > "$tmp" 2>/dev/null && [ -s "$tmp" ]; then
      cp "$LINK" "$LINK.bak-dropbridge" 2>/dev/null || true
      cp "$tmp" "$LINK"
    else
      echo "   WARN: could not update $LINK (left unchanged)."
    fi
    rm -f "$tmp"
    if [ -f "$ORDER" ]; then
      tmp=$(mktemp) || tmp=/tmp/dropbridge-order.$$
      if jq '.data |= ((. - ["dropbridge","DropBridge"]) + ["DropBridge"])' "$ORDER" > "$tmp" 2>/dev/null && [ -s "$tmp" ]; then
        cp "$tmp" "$ORDER"
      fi
      rm -f "$tmp"
    fi
    systemctl restart zimaos-app-management 2>/dev/null || true
    echo "-> fixed-URL dashboard tile installed -> $TSURL"
  else
    echo "-> NOTE: could not derive this node's tailnet URL (is Tailscale up & jq present?)."
    echo "         The app IS running; open it at  http://<this-node-tailnet-ip>:$PORT/"
  fi
fi

cat <<EOF

==================================================================
 DropBridge is up on port $PORT (standalone, restart: unless-stopped).

 !! Reach it over your TAILNET (the control plane rejects LAN/Internet) !!
    On ZimaOS the dashboard tile is a fixed-URL LinkApp, so it opens the
    tailnet origin no matter how you reached the dashboard. Your browsing
    device must be on the tailnet.

 Edit node identity:  $DEPLOY/.env  then
   (cd $DEPLOY && $DC compose up -d --force-recreate)

 Logs:       $DC logs -f dropbridge
 Uninstall:  cd $DEPLOY && $DC compose down
   # remove the tile: delete the "DropBridge" entry from $LINK,
   # then: systemctl restart zimaos-app-management
==================================================================
EOF
