"""After the realm import: give user tokens issued through host-login a may_act claim naming the
HOST's service account, which Keycloak's delegation profile requires before idp-host may act for
the user (ADR-0001 D16). DEV ONLY, driven by deploy/e2e.sh.

Usage: python3 -I deploy/e2e_keycloak_setup.py KEYCLOAK_BASE ADMIN_USER ADMIN_PASSWORD
"""
import json
import sys
import urllib.parse
import urllib.request

base, admin_user, admin_password = sys.argv[1:4]
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def call(method, path, body=None, token=None, form=None):
    data = None
    headers = {'Accept': 'application/json'}
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers['Content-Type'] = 'application/x-www-form-urlencoded'
    elif body is not None:
        data = json.dumps(body).encode()
        headers['Content-Type'] = 'application/json'
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(base + path, data=data, method=method, headers=headers)
    with opener.open(req, timeout=60) as resp:
        raw = resp.read()
        return resp.status, (json.loads(raw) if raw else None)


_, tok = call('POST', '/realms/master/protocol/openid-connect/token',
              form={'grant_type': 'password', 'client_id': 'admin-cli', 'username': admin_user, 'password': admin_password})
admin = tok['access_token']
_, clients = call('GET', '/admin/realms/idp/clients?clientId=idp-host', token=admin)
host_client = clients[0]['id']
_, sa = call('GET', f'/admin/realms/idp/clients/{host_client}/service-account-user', token=admin)
actor_sub = sa['id']
_, clients = call('GET', '/admin/realms/idp/clients?clientId=host-login', token=admin)
login_client = clients[0]['id']
status, _ = call('POST', f'/admin/realms/idp/clients/{login_client}/protocol-mappers/models', token=admin, body={
    'name': 'may_act', 'protocol': 'openid-connect', 'protocolMapper': 'oidc-hardcoded-claim-mapper',
    'config': {'claim.name': 'may_act', 'claim.value': json.dumps({'sub': actor_sub, 'client_id': 'idp-host'}), 'jsonType.label': 'JSON',
               'access.token.claim': 'true', 'id.token.claim': 'false', 'introspection.token.claim': 'true'}})
print(f'OK: may_act -> idp-host service account {actor_sub} (HTTP {status})')
