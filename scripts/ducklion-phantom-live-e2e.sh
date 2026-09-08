#!/usr/bin/env bash
# Full isolated live E2E:
# real OAuth -> Duckway server -> phantom client sync -> proxy ->
# Ducklord -> SSH -> Ducklion -> Codex and Claude PTYs.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
duckway_init_container_runtime
RUNTIME="$CONTAINER_RUNTIME"
CODEX_AUTH="${CODEX_AUTH:-$ROOT/live-credentials/codex-auth.json}"
CLAUDE_AUTH="${CLAUDE_AUTH:-$ROOT/live-credentials/claude-credentials.json}"
KEEP="${DUCKWAY_LIVE_KEEP:-0}"
RUN_ID="${DUCKWAY_LIVE_RUN_ID:-$(date +%s)-$$}"
SAFE_ID="$(printf '%s' "$RUN_ID" | tr -cd 'a-zA-Z0-9_.-' | cut -c1-32)"
WORK="$(mktemp -d -t duckway-phantom-e2e-XXXXXX)"
NET="dw-live-$SAFE_ID"
SERVER="dw-server-$SAFE_ID"
AGENT="dw-agent-$SAFE_ID"
LORD="dw-lord-$SAFE_ID"
IMAGE="duckway-live-e2e:$SAFE_ID"
SUCCESS=0

validate_credential() {
  local name="$1" path="$2" mode
  if [ -L "$path" ] || [ ! -f "$path" ]; then
    echo "$name credential must be a regular non-symlink file: $path" >&2
    exit 1
  fi
  mode="$(stat -c '%a' "$path" 2>/dev/null || stat -f '%Lp' "$path")"
  if [ "$mode" != 600 ]; then
    echo "$name credential must have mode 0600: $path (got $mode)" >&2
    exit 1
  fi
}
validate_credential Codex "$CODEX_AUTH"
validate_credential Claude "$CLAUDE_AUTH"
CODEX_AUTH="$(cd "$(dirname "$CODEX_AUTH")" && pwd)/$(basename "$CODEX_AUTH")"
CLAUDE_AUTH="$(cd "$(dirname "$CLAUDE_AUTH")" && pwd)/$(basename "$CLAUDE_AUTH")"

cleanup() {
  if [ "$KEEP" = 1 ] && [ "$SUCCESS" = 1 ]; then
    echo "[live-e2e] retained demo: server=$SERVER agent=$AGENT controller=$LORD network=$NET"
    echo "[live-e2e] open TUI: $RUNTIME exec -it $LORD ducklord tui"
    echo "[live-e2e] interactive sessions: codex-demo, claude-demo"
    echo "[live-e2e] cleanup: $RUNTIME rm -f $LORD $AGENT $SERVER && $RUNTIME network rm $NET && $RUNTIME image rm $IMAGE && rm -rf $WORK"
    return
  fi
  "$RUNTIME" rm -f "$LORD" "$AGENT" "$SERVER" >/dev/null 2>&1 || true
  "$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
  "$RUNTIME" image rm "$IMAGE" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

mkdir -p "$WORK/shared" "$WORK/secrets" "$WORK/build" "$WORK/data"
chmod 700 "$WORK" "$WORK/shared" "$WORK/secrets" "$WORK/build" "$WORK/data"
python3 - "$CODEX_AUTH" "$CLAUDE_AUTH" "$WORK/secrets" <<'PY'
import os, stat, sys
for source, name in zip(sys.argv[1:3], ('codex-auth.json', 'claude-credentials.json')):
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0)
    fd = os.open(source, flags)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600:
            raise SystemExit(f'unsafe live credential metadata: {source}')
        target = os.path.join(sys.argv[3], name)
        out = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            while True:
                chunk = os.read(fd, 65536)
                if not chunk: break
                os.write(out, chunk)
            os.fsync(out)
        finally:
            os.close(out)
    finally:
        os.close(fd)
PY
CODEX_AUTH="$WORK/secrets/codex-auth.json"
CLAUDE_AUTH="$WORK/secrets/claude-credentials.json"
python3 - "$CODEX_AUTH" "$CLAUDE_AUTH" <<'PY'
import base64, json, sys, time
codex=json.load(open(sys.argv[1])); tokens=codex.get('tokens',codex)
access=tokens.get('access_token') or codex.get('access_token') or ''
try:
    payload=access.split('.')[1]; payload += '='*(-len(payload)%4)
    codex_exp=int(json.loads(base64.urlsafe_b64decode(payload))['exp'])*1000
except Exception: raise SystemExit('Codex access token is missing or malformed')
claude=json.load(open(sys.argv[2])).get('claudeAiOauth',{})
deadline=int((time.time()+1800)*1000)
if codex_exp <= deadline: raise SystemExit('Codex access token expires within 30 minutes; refresh live-credentials first')
if int(claude.get('expiresAt') or 0) <= deadline: raise SystemExit('Claude access token expires within 30 minutes; refresh live-credentials first')
PY
echo '[live-e2e] building isolated image'
CGO_ENABLED=0 go build -o "$WORK/build/duckway-server" "$ROOT/cmd/server"
CGO_ENABLED=0 go build -o "$WORK/build/duckway" "$ROOT/cmd/client"
CGO_ENABLED=0 go build -o "$WORK/build/ducklion" "$ROOT/cmd/ducklion"
CGO_ENABLED=0 go build -o "$WORK/build/ducklord" "$ROOT/cmd/ducklord"
ssh-keygen -q -t ed25519 -N '' -f "$WORK/build/id_ed25519"

cat >"$WORK/build/Containerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache bash ca-certificates curl jq nodejs npm openssh openssh-client python3 \
 && npm install -g @openai/codex@0.153.4 @anthropic-ai/claude-code@2.1.263 \
 && npm cache clean --force \
 && ssh-keygen -A
COPY duckway-server duckway ducklion ducklord /usr/local/bin/
COPY id_ed25519.pub /root/.ssh/authorized_keys
RUN chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
EOF
"$RUNTIME" build -q -t "$IMAGE" -f "$WORK/build/Containerfile" "$WORK/build" >/dev/null
"$RUNTIME" network create "$NET" >/dev/null

echo '[live-e2e] starting credential-isolated server'
"$RUNTIME" run -d --name "$SERVER" --hostname server --network "$NET" \
  --mount "type=bind,src=$CODEX_AUTH,dst=/run/secrets/codex-auth.json,readonly" \
  --mount "type=bind,src=$CLAUDE_AUTH,dst=/run/secrets/claude-credentials.json,readonly" \
  --mount "type=bind,src=$WORK/shared,dst=/shared" \
  --mount "type=bind,src=$WORK/data,dst=/data" \
  -e DUCKWAY_DATA_DIR=/data -e DUCKWAY_LISTEN=0.0.0.0:19090 \
  "$IMAGE" duckway-server --data /data --listen 0.0.0.0:19090 >/dev/null
for _ in $(seq 1 120); do
  if "$RUNTIME" exec "$SERVER" curl -fsS http://127.0.0.1:19090/version >/dev/null 2>&1; then break; fi
  sleep .25
done
"$RUNTIME" exec "$SERVER" curl -fsS http://127.0.0.1:19090/version >/dev/null
ADMIN_PASS="$($RUNTIME logs "$SERVER" 2>&1 | sed -n 's/.*Password: //p' | tail -n 1)"
if [ -z "$ADMIN_PASS" ]; then echo 'could not obtain ephemeral admin password' >&2; exit 1; fi

echo '[live-e2e] importing OAuth credentials and registering client'
"$RUNTIME" exec -i "$SERVER" sh -eu -s -- "$SAFE_ID" "$ADMIN_PASS" <<'SERVER_SEED'
run_id="$1"; admin_pass="$2"; base=http://127.0.0.1:19090; cookie=/tmp/cookie
jq -n --arg password "$admin_pass" '{username:"duckway",password:$password}' >/tmp/login.json
curl -fsS -c "$cookie" -X POST "$base/api/auth/login" -H 'Content-Type: application/json' --data-binary @/tmp/login.json >/dev/null
python3 - "$base" "$cookie" "$run_id" <<'PY'
import base64, json, subprocess, sys, time
base, cookie, run_id = sys.argv[1:]
def curl(method, path, payload=None):
    cmd=["curl","-fsS","-b",cookie,"-X",method,base+path]
    if payload is not None: cmd += ["-H","Content-Type: application/json","--data-binary",json.dumps(payload)]
    return json.loads(subprocess.check_output(cmd))
services={x["name"]:x["id"] for x in curl("GET","/api/services")}
with open('/run/secrets/codex-auth.json') as f: codex=json.load(f)
ct=codex.get('tokens',codex)
access=ct.get('access_token') or codex.get('access_token'); refresh=ct.get('refresh_token') or codex.get('refresh_token'); ident=ct.get('id_token') or codex.get('id_token')
if not all((access,refresh,ident)): raise SystemExit('Codex credential fields missing')
try:
    p=access.split('.')[1]; p += '='*(-len(p)%4); exp=int(json.loads(base64.urlsafe_b64decode(p))['exp'])*1000
except Exception: exp=int((time.time()+3600)*1000)
if exp <= int((time.time()+1800)*1000): raise SystemExit('Codex access token expires within 30 minutes; refresh live-credentials first')
sub={'credential_kind':'codex_oauth','auth_mode':codex.get('auth_mode','chatgpt'),'source':'codex','id_token':ident}
for k in ('account_id','last_refresh'):
    if ct.get(k) or codex.get(k): sub[k]=ct.get(k) or codex.get(k)
cp={'service_id':services['openai'],'name':run_id+' codex','access_token':access,'refresh_token':refresh,'expires_at':exp,'token_endpoint':'https://auth.openai.com/oauth/token','subscription_info':json.dumps(sub,separators=(',',':'))}
curl('POST','/api/oauth/validate',cp); codex_key=curl('POST','/api/oauth/upload',cp)['id']
with open('/run/secrets/claude-credentials.json') as f: claude=json.load(f)
co=claude.get('claudeAiOauth',{})
if not co.get('accessToken') or not co.get('refreshToken'): raise SystemExit('Claude credential fields missing')
if int(co.get('expiresAt') or 0) <= int((time.time()+1800)*1000): raise SystemExit('Claude access token expires within 30 minutes; refresh live-credentials first')
csub={k:co[k] for k in ('subscriptionType','rateLimitTier','scopes') if k in co}
clp={'service_id':services['anthropic'],'name':run_id+' claude','access_token':co['accessToken'],'refresh_token':co['refreshToken'],'expires_at':co.get('expiresAt',0),'token_endpoint':'https://console.anthropic.com/v1/oauth/token','subscription_info':json.dumps(csub,separators=(',',':'))}
curl('POST','/api/oauth/validate',clp); claude_key=curl('POST','/api/oauth/upload',clp)['id']
client=curl('POST','/api/clients',{'name':run_id+' agent'})
codex_ph=curl('POST','/api/placeholders',{'service_id':services['openai'],'api_key_id':codex_key,'client_id':client['id'],'env_name':'OPENAI_API_KEY','requires_approval':False})['placeholder']
claude_ph=curl('POST','/api/placeholders',{'service_id':services['anthropic'],'api_key_id':claude_key,'client_id':client['id'],'env_name':'ANTHROPIC_AUTH_TOKEN','requires_approval':False})['placeholder']
with open('/shared/provision.json.tmp','w') as f:
    json.dump({'client_id':client['id'],'token':client['token'],'codex_phantom':codex_ph,'claude_phantom':claude_ph},f)
import os
os.chmod('/shared/provision.json.tmp',0o600); os.replace('/shared/provision.json.tmp','/shared/provision.json')
PY
new_pass="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
jq -n --arg old "$admin_pass" --arg new "$new_pass" '{current_password:$old,new_password:$new}' >/tmp/change.json
curl -fsS -b "$cookie" -X POST "$base/api/auth/change-password" -H 'Content-Type: application/json' --data-binary @/tmp/change.json >/dev/null
curl -fsS -b "$cookie" -X POST "$base/api/auth/logout" >/dev/null
SERVER_SEED

# The retained/running server must not keep access to host credential files.
"$RUNTIME" container rm --force "$SERVER" >/dev/null
unlink "$CODEX_AUTH"
unlink "$CLAUDE_AUTH"
"$RUNTIME" run -d --name "$SERVER" --hostname server --network "$NET" \
  --mount "type=bind,src=$WORK/data,dst=/data" \
  -e DUCKWAY_DATA_DIR=/data -e DUCKWAY_LISTEN=0.0.0.0:19090 \
  "$IMAGE" duckway-server --data /data --listen 0.0.0.0:19090 >/dev/null
for _ in $(seq 1 120); do
  if "$RUNTIME" exec "$SERVER" curl -fsS http://127.0.0.1:19090/version >/dev/null 2>&1; then break; fi
  sleep .25
done
"$RUNTIME" exec "$SERVER" curl -fsS http://127.0.0.1:19090/version >/dev/null
"$RUNTIME" exec "$SERVER" test ! -e /run/secrets/codex-auth.json
"$RUNTIME" exec "$SERVER" test ! -e /run/secrets/claude-credentials.json

echo '[live-e2e] starting phantom-only Ducklion host and Ducklord controller'
"$RUNTIME" run -d --name "$AGENT" --hostname agent --network "$NET" "$IMAGE" sleep infinity >/dev/null
"$RUNTIME" run -d --name "$LORD" --hostname controller --network "$NET" "$IMAGE" sleep infinity >/dev/null
"$RUNTIME" cp "$WORK/shared/provision.json" "$AGENT":/tmp/provision.json
"$RUNTIME" cp "$WORK/build/id_ed25519" "$LORD":/root/.ssh/id_ed25519

"$RUNTIME" exec -i "$AGENT" sh -eu -s <<'AGENT_SETUP'
test ! -e /run/secrets/codex-auth.json
test ! -e /run/secrets/claude-credentials.json
home=/root; cfg=$home/.duckway; base=http://server:19090
mkdir -p "$cfg" /workspace
status=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$base/api/auth/login" -H 'Content-Type: application/json' -d '{"username":"duckway","password":"duckway"}')
test "$status" = 401
status=$(curl -sS -o /dev/null -w '%{http_code}' -H 'X-Duckway-Token: invalid-e2e-token' "$base/client/sync")
test "$status" = 401
token=$(jq -r .token /tmp/provision.json)
name=$(jq -r .client_id /tmp/provision.json)
cat >"$cfg/config.yaml" <<EOF
server_url: $base
client_name: $name
token: $token
proxy_port: 18080
EOF
chmod 600 "$cfg/config.yaml"
curl -fsS -o "$cfg/ca.pem" "$base/skill/ca.pem"
curl -fsS -H "X-Duckway-Token: $token" -o "$cfg/ca-key.pem" "$base/client/ca-key"
chmod 600 "$cfg/ca-key.pem"
duckway sync >/tmp/sync.log 2>&1
python3 - <<'PY'
import json, os
p=json.load(open('/tmp/provision.json'))
env={}
for line in open('/root/.duckway/keys.env'):
    if '=' in line:
        k,v=line.rstrip('\n').split('=',1); env[k]=v.strip("'\"")
if env.get('OPENAI_API_KEY') != p['codex_phantom']: raise SystemExit('Codex phantom assignment mismatch')
if env.get('ANTHROPIC_AUTH_TOKEN') != p['claude_phantom']: raise SystemExit('Claude phantom assignment mismatch')
if os.stat('/root/.codex/auth.json').st_mode & 0o777 != 0o600: raise SystemExit('Codex auth mode is not 0600')
if os.stat('/root/.claude/.credentials.json').st_mode & 0o777 != 0o600: raise SystemExit('Claude auth mode is not 0600')
# Demo sessions use a known empty workspace. Pre-approve that exact path so
# retained interactive CLIs open directly at their prompt rather than at a
# first-run dialog that automation could accidentally answer incorrectly.
claude_path='/root/.claude.json'
claude=json.load(open(claude_path)) if os.path.exists(claude_path) else {}
claude.setdefault('projects', {}).setdefault('/workspace', {})['hasTrustDialogAccepted']=True
tmp=claude_path+'.tmp'
with open(tmp, 'w') as f: json.dump(claude, f)
os.chmod(tmp, 0o600); os.replace(tmp, claude_path)
PY
cat >>/root/.codex/config.toml <<'EOF'

[projects."/workspace"]
trust_level = "trusted"
EOF
cp "$cfg/ca.pem" /usr/local/share/ca-certificates/duckway.crt
update-ca-certificates >/dev/null
set -a; . "$cfg/keys.env"; set +a
export HTTP_PROXY=http://127.0.0.1:18080 HTTPS_PROXY=http://127.0.0.1:18080 NO_PROXY=localhost,127.0.0.1,server
export NODE_EXTRA_CA_CERTS="$cfg/ca.pem"
nohup duckway proxy --debug >/tmp/proxy.log 2>&1 </dev/null &
echo $! >"$cfg/proxy.pid"
chmod 600 "$cfg/proxy.pid"
for _ in $(seq 1 120); do
  if grep -q 'Duckway proxy listening' /tmp/proxy.log; then break; fi
  sleep .1
done
grep -q 'Duckway proxy listening' /tmp/proxy.log
cat >/usr/local/bin/ducklion-live <<EOF
#!/bin/sh
export HOME=/root DUCKWAY_CONFIG_DIR=/root/.duckway
export TERM=xterm-256color COLORTERM=truecolor
export HTTP_PROXY=http://127.0.0.1:18080 HTTPS_PROXY=http://127.0.0.1:18080 NO_PROXY=localhost,127.0.0.1,server
export NODE_EXTRA_CA_CERTS=/root/.duckway/ca.pem
exec /usr/local/bin/ducklion "\$@"
EOF
chmod 755 /usr/local/bin/ducklion-live
nohup ducklion-live daemon >/tmp/ducklion.log 2>&1 </dev/null &
/usr/sbin/sshd
AGENT_SETUP

"$RUNTIME" exec -i "$LORD" sh -eu -s <<'LORD_SETUP'
chmod 600 /root/.ssh/id_ed25519
cat >/root/.ssh/config <<'EOF'
Host live-agent
  HostName agent
  User root
  IdentityFile /root/.ssh/id_ed25519
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF
chmod 600 /root/.ssh/config
mkdir -p /root/.ducklord
cat >/root/.ducklord/config.yaml <<'EOF'
name: live-controller
hosts:
  - name: live-agent
    host: live-agent
    user: root
    ducklion: /usr/local/bin/ducklion-live
EOF
LORD_SETUP

for _ in $(seq 1 120); do
  if "$RUNTIME" exec "$LORD" ducklord probe live-agent >/dev/null 2>&1; then break; fi
  sleep .25
done
"$RUNTIME" exec "$LORD" ducklord probe live-agent >/dev/null

nonce="DUCKWAY_E2E_${SAFE_ID}_OK"
echo '[live-e2e] running Codex PTY'
"$RUNTIME" exec "$LORD" ducklord start live-agent --name codex-live --agent codex --cwd /workspace --config /root/.ducklord/config.yaml -- codex exec --json --skip-git-repo-check --sandbox read-only -C /workspace "Reply exactly: $nonce"
echo '[live-e2e] running Claude PTY'
"$RUNTIME" exec "$LORD" ducklord start live-agent --name claude-live --agent claude_code --cwd /workspace --config /root/.ducklord/config.yaml -- claude --print --output-format json "Reply exactly: $nonce"

for handle in codex-live claude-live; do
  done_flag=0
  for _ in $(seq 1 360); do
    if "$RUNTIME" exec "$LORD" ducklord sessions live-agent | grep -q "$handle.*stopped"; then done_flag=1; break; fi
    sleep .5
  done
  if [ "$done_flag" != 1 ]; then echo "$handle did not stop" >&2; exit 1; fi
  "$RUNTIME" exec "$LORD" ducklord read live-agent "$handle" --lines 240 >"$WORK/$handle.out"
done
python3 - "$WORK/codex-live.out" "$nonce" <<'PY'
import json, re, sys
events=[]
for line in re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', open(sys.argv[1], errors='replace').read()).splitlines():
    try: events.append(json.loads(line.rstrip('\r')))
    except json.JSONDecodeError: pass
if any(e.get('type') in ('turn.failed','error') for e in events): raise SystemExit('Codex emitted a failure event')
if not any(e.get('type') == 'turn.completed' for e in events): raise SystemExit('Codex did not complete its turn')
texts=[]
for e in events:
    item=e.get('item') if isinstance(e.get('item'),dict) else {}
    if item.get('type') == 'agent_message' and isinstance(item.get('text'),str): texts.append(item['text'].strip())
if sys.argv[2] not in texts: raise SystemExit('Codex assistant response did not exactly match the nonce')
PY
if ! "$RUNTIME" exec "$AGENT" grep -q 'openai-chatgpt' /tmp/proxy.log; then echo 'proxy saw no Codex traffic' >&2; exit 1; fi
python3 - "$WORK/claude-live.out" "$nonce" <<'PY'
import json, re, sys
objects=[]
for line in re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', open(sys.argv[1], errors='replace').read()).splitlines():
    try: objects.append(json.loads(line.rstrip('\r')))
    except json.JSONDecodeError: pass
results=[o for o in objects if o.get('type') == 'result']
if len(results) != 1 or results[0].get('is_error') or results[0].get('result','').strip() != sys.argv[2]:
    raise SystemExit('Claude did not return one successful exact nonce result')
PY
if ! "$RUNTIME" exec "$AGENT" grep -q 'anthropic' /tmp/proxy.log; then echo 'proxy saw no Claude traffic' >&2; exit 1; fi

if [ "$KEEP" = 1 ]; then
  echo '[live-e2e] starting retained interactive Codex and Claude PTYs'
  "$RUNTIME" exec "$LORD" ducklord start live-agent --name codex-demo --agent codex --cwd /workspace --config /root/.ducklord/config.yaml -- codex --sandbox workspace-write -C /workspace
  "$RUNTIME" exec "$LORD" ducklord start live-agent --name claude-demo --agent claude_code --cwd /workspace --config /root/.ducklord/config.yaml -- claude
  for handle in codex-demo claude-demo; do
    running=0
    for _ in $(seq 1 120); do
      if "$RUNTIME" exec "$LORD" ducklord sessions live-agent | grep -q "$handle.*running"; then running=1; break; fi
      sleep .25
    done
    if [ "$running" != 1 ]; then echo "$handle did not remain interactive" >&2; exit 1; fi
  done
  for handle in codex-demo claude-demo; do
    ready=0
    for _ in $(seq 1 120); do
      "$RUNTIME" exec "$LORD" ducklord read live-agent "$handle" --lines 240 >"$WORK/$handle.out"
      if python3 - "$WORK/$handle.out" "$handle" <<'PY'
import sys
data=open(sys.argv[1], 'rb').read().lower()
blocked=(b'do you trust the contents' in data or b'quick safety check' in data or
         b'login required' in data or b'authentication failed' in data)
if blocked: raise SystemExit(2)
marker=b'ask codex to do anything' if sys.argv[2] == 'codex-demo' else b'claude code'
raise SystemExit(0 if marker in data else 1)
PY
      then
        ready=1
        break
      else
        status=$?
        if [ "$status" = 2 ]; then echo "$handle stopped at a trust/authentication screen" >&2; exit 1; fi
      fi
      sleep .25
    done
    if [ "$ready" != 1 ]; then echo "$handle did not reach its interactive prompt" >&2; exit 1; fi
  done
  python3 - "$WORK/codex-demo.out" "$WORK/claude-demo.out" <<'PY'
import re, sys
for path in sys.argv[1:]:
    data=open(path, 'rb').read()
    if not re.search(rb'\x1b\[[0-9;:]*m', data):
        raise SystemExit(f'interactive PTY emitted no SGR color/style output: {path}')
PY
fi

SUCCESS=1
echo '[live-e2e] PASS: server import, phantom sync, Ducklord, Ducklion, Codex, and Claude'
