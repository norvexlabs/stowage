#!/usr/bin/env python3
"""A handoff's run and check: environment in, KEY=VALUE lines out, as the provision hook.

issue prints what `gives` lists, for the instance STOWAGE_KIT_INSTANCE, from what `ask` asked. check
says whether a kit was taken, by its exit status: 0 proven, 2 unknown, any other not proven; this
stand-in knows no site, so the instance's name says.
"""
import os
import sys

instance = os.environ["STOWAGE_KIT_INSTANCE"]
if sys.argv[1:] == ["issue"]:
    if not os.environ.get("SITE_SEARCH_TOKEN"):
        sys.exit("SITE_SEARCH_TOKEN is required")
    print("SEARCH_URL=" + os.environ["SITE_SEARCH_URL"])
elif sys.argv[1:] == ["check"]:
    sys.exit({"site-unknown": 2, "site-refused": 1}.get(instance, 0))
else:
    sys.exit("usage: handoff.py issue|check")
