"""Provision the MCP Infrastructure for the HOST end-to-end run (DEV ONLY, driven by deploy/e2e.sh):
register and approve the two upstreams, grant their tools to the operator role, mint an API key for
the HOST's service identity, compile, approve and deploy a release, and print the key.

Usage: python3 -I deploy/e2e_mcp_setup.py REGISTRY_URL GATEWAY_ID ADMIN_TOKEN OPERATOR_TOKEN REVIEWER_TOKEN
"""
import json
import sys
import time
import urllib.error
import urllib.request

reg, gateway_id, admin, operator, reviewer = sys.argv[1:6]
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(token, method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(reg + path, data=data, method=method,
                                 headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json', 'Accept': 'application/json'})
    try:
        with opener.open(req, timeout=120) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, (json.loads(raw) if raw else {})


def must(token, method, path, body=None, want=(200, 201)):
    status, out = api(token, method, path, body)
    if status not in want:
        print(f'{method} {path}: HTTP {status} {out}', file=sys.stderr)
        sys.exit(2)
    return out


ids = {}
for name, endpoint in (('minimal', 'http://minimal:3111/mcp'), ('reference', 'http://reference:3221/mcp')):
    s = must(operator, 'POST', '/api/v1/servers', {'displayName': name, 'targetName': name, 'endpoint': endpoint})
    ids[name] = s['serverId']
    must(operator, 'POST', f'/api/v1/servers/{ids[name]}/discover')
    must(reviewer, 'POST', f'/api/v1/servers/{ids[name]}/approve')
for body in ({'serverId': ids['minimal'], 'tools': ['echo', 'sum'], 'roles': ['operator']},
             {'serverId': ids['reference'], 'tools': ['test_simple_text'], 'roles': ['operator']}):
    g = must(operator, 'POST', '/api/v1/grants', body)
    must(reviewer, 'POST', f"/api/v1/grants/{g['grantId']}/approve")
key = must(admin, 'POST', f'/api/v1/gateways/{gateway_id}/api-keys', {'subject': 'svc-host', 'roles': ['operator']})['key']
rel = must(operator, 'POST', '/api/v1/releases', {'gatewayId': gateway_id})
must(reviewer, 'POST', f"/api/v1/releases/{rel['releaseId']}/approve")
must(admin, 'POST', f"/api/v1/releases/{rel['releaseId']}/deploy")
for _ in range(60):
    if must(reviewer, 'GET', f"/api/v1/releases/{rel['releaseId']}")['status'] == 'GOOD':
        break
    time.sleep(1)
else:
    print('release never became GOOD', file=sys.stderr)
    sys.exit(2)
print(key)
