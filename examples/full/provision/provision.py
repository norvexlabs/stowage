#!/usr/bin/env python3
"""Provision hook contract: environment in, KEY=VALUE lines out, idempotent and resumable.

It receives what hooks.provision.env lists (ADMIN_EMAIL, ADMIN_PASSWORD, INTERNAL_SERVICE_SECRET)
and what it returned before; API_URL (default http://api:9090) comes from the chart's ConfigMap
on Kubernetes. The administrator's password is ADMIN_PASSWORD when given (an input on Kubernetes;
on Docker the one this hook returned before), else minted here and printed before the step that
needs it — a password given is never printed. The administrator's API key is minted at the first
run (ADMIN_API_KEY) and registered with the administrator through the api; ADMIN_LOGIN is printed
once the api has registered both. A later run checks the key it returned: one the api no longer
knows (revoked by the app's own users, or data restored from elsewhere) fails the run, naming the
`provision --set` that unblocks it; the spec declares ADMIN_API_KEY in hooks.provision.returns, so
`provision --set ADMIN_API_KEY=<key>` gives a key this hook never returned (its record lost). The api is retried for a minute while it is not up yet. No
secret is put on a command line.
"""
import json
import os
import secrets
import string
import sys
import time
import urllib.error
import urllib.request

email = os.environ.get("ADMIN_EMAIL") or sys.exit("ADMIN_EMAIL is required")
token = os.environ.get("INTERNAL_SERVICE_SECRET") or sys.exit("INTERNAL_SERVICE_SECRET is required")
api = os.environ.get("API_URL") or "http://api:9090"
password = os.environ.get("ADMIN_PASSWORD")
key = os.environ.get("ADMIN_API_KEY")
registered = os.environ.get("ADMIN_LOGIN") == email


def call(req, what):
    """The api's answer to req: retried while the api cannot be reached, for a minute."""
    deadline = time.monotonic() + 60
    while True:
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status
        except urllib.error.HTTPError as e:
            if e.code == 401:
                return 401
            sys.exit(f"the api refused {what}: {e.code} {e.read(512).decode(errors='replace')}")
        except OSError as e:
            if time.monotonic() > deadline:
                sys.exit(f"the api cannot be reached at {api}: {e}")
            time.sleep(1)


if not password:
    password = "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(20))
    print(f"ADMIN_PASSWORD={password}", flush=True)
if key and registered:
    check = urllib.request.Request(f"{api}/api/v1/key", headers={"Authorization": f"Bearer {key}"})
    if call(check, "the key check") == 401:
        sys.exit("the api does not know the ADMIN_API_KEY this hook returned before (revoked, or data from elsewhere): "
                 "`provision --set ADMIN_API_KEY=` forgets it, and the run mints another; "
                 "`provision --set ADMIN_API_KEY=<a key the api knows>` records that one")
if not key:
    key = secrets.token_urlsafe(24)
    print(f"ADMIN_API_KEY={key}", flush=True)
print(f"registering {email}", file=sys.stderr, flush=True)
req = urllib.request.Request(f"{api}/api/v1/admin", method="POST",
                             data=json.dumps({"email": email, "password": password, "api_key": key}).encode(),
                             headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"})
if call(req, f"the administrator {email}") == 401:
    sys.exit(f"the api refused the administrator {email}: the internal service secret")
print(f"the api registered {email}", file=sys.stderr)
print(f"ADMIN_LOGIN={email}", flush=True)
