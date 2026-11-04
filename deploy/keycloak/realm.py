#!/usr/bin/env python3
"""Generate the development Keycloak realm for the HOST's MODE=oidc end-to-end run (ADR-0001 D16).

Real users log in with the authorization code flow + PKCE through host-app (a public client, like
a Workspace front end; D25); their tokens carry tenant_id (user attribute), roles (realm roles) and
the audience idp-host. The HOST is the confidential client idp-host: standard token exchange enabled,
audience mapper for the gateway resource, so a user's token can be exchanged (with the HOST's own
token as actor_token) for a token the gateway accepts, carrying act = idp-host. The may_act claim
the delegation profile requires on the user's token is added after import by
deploy/e2e_keycloak_setup.py (it needs the service-account user id Keycloak assigns).
eer-intelligence is the workload identity of EER's Intelligence worker (ADR-036 D21, D25): a
confidential client with a service account whose client-credentials token carries aud idp-host,
the tenant and the role host-user as hardcoded claims - no user is involved.

  python3 deploy/keycloak/realm.py OUT_DIR TENANT_ID RESOURCE_URI CLIENTS_FILE
writes OUT_DIR/idp-realm.json (mounted for --import-realm) and CLIENTS_FILE (secrets for the test).
CLIENTS_FILE must not be in OUT_DIR: Keycloak imports every .json file of that directory as a realm.
"""
import json
import os
import secrets
import sys

out, tenant, resource, clients_file = sys.argv[1:5]


def hardcoded(name, value, json_type="String"):
    return {"name": name, "protocol": "openid-connect", "protocolMapper": "oidc-hardcoded-claim-mapper",
            "config": {"claim.name": name, "claim.value": value, "jsonType.label": json_type,
                       "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}


def audience(a):
    return {"name": "aud-" + a[-24:], "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "config": {"included.custom.audience": a, "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}


tenant_attr = {"name": "tenant_id", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper",
               "config": {"user.attribute": "tenant_id", "claim.name": "tenant_id", "jsonType.label": "String",
                          "access.token.claim": "true", "id.token.claim": "true", "userinfo.token.claim": "true", "introspection.token.claim": "true"}}
roles_map = {"name": "roles", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-realm-role-mapper",
             "config": {"claim.name": "roles", "jsonType.label": "String", "multivalued": "true",
                        "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}

users = {"alice": ["host-user", "operator"], "hostadmin": ["host-admin"], "hostapprover": ["host-approver"], "bob": ["host-user", "viewer"]}
passwords = {u: secrets.token_urlsafe(16) for u in users}
host_secret = secrets.token_urlsafe(32)
eer_secret = secrets.token_urlsafe(32)
realm = {
    "realm": "idp", "enabled": True, "sslRequired": "none", "accessTokenLifespan": 600,
    "roles": {"realm": [{"name": r} for r in ("host-user", "host-admin", "host-approver", "operator", "viewer")]},
    "users": [{"username": u, "enabled": True, "emailVerified": True, "email": u + "@example.com", "firstName": u, "lastName": "User",
               "attributes": {"tenant_id": [tenant]}, "realmRoles": roles,
               "credentials": [{"type": "password", "value": passwords[u], "temporary": False}]} for u, roles in users.items()],
    "clients": [
        {"clientId": "host-app", "name": "user login (authorization code + PKCE)", "enabled": True, "protocol": "openid-connect", "publicClient": True,
         "standardFlowEnabled": True, "directAccessGrantsEnabled": False, "redirectUris": ["http://127.0.0.1:18080/callback"],
         "fullScopeAllowed": True, "attributes": {"pkce.code.challenge.method": "S256"},
         "protocolMappers": [audience("idp-host"), tenant_attr, roles_map]},
        {"clientId": "eer-intelligence", "name": "EER Intelligence worker (workload identity, client credentials)", "enabled": True, "protocol": "openid-connect",
         "publicClient": False, "secret": eer_secret, "standardFlowEnabled": False, "directAccessGrantsEnabled": False, "serviceAccountsEnabled": True,
         "fullScopeAllowed": False, "protocolMappers": [audience("idp-host"), hardcoded("tenant_id", tenant), hardcoded("roles", json.dumps(["host-user"]), "JSON")]},
        {"clientId": "idp-host", "name": "IDP HOST (token exchange requester, delegation actor)", "enabled": True, "protocol": "openid-connect",
         "publicClient": False, "secret": host_secret, "standardFlowEnabled": False, "serviceAccountsEnabled": True, "fullScopeAllowed": True,
         "attributes": {"standard.token.exchange.enabled": "true"}, "protocolMappers": [audience(resource), tenant_attr, roles_map]},
        {"clientId": resource, "name": "gateway resource (audience only)", "enabled": True, "protocol": "openid-connect", "bearerOnly": True,
         "publicClient": False, "standardFlowEnabled": False},
    ],
}
os.makedirs(out, exist_ok=True)
with open(os.path.join(out, "idp-realm.json"), "w") as f:
    json.dump(realm, f, indent=1)
os.chmod(os.path.join(out, "idp-realm.json"), 0o644)  # read by the Keycloak container user; dev secrets only
with open(clients_file, "w") as f:
    json.dump({"users": passwords, "host-app": {"clientId": "host-app"}, "idp-host": {"clientId": "idp-host", "secret": host_secret},
               "eer-intelligence": {"clientId": "eer-intelligence", "secret": eer_secret}}, f, indent=1)
os.chmod(clients_file, 0o600)
print("OK: realm idp with", len(realm["clients"]), "clients and", len(users), "users")
