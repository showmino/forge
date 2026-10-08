"""Run 'forge auth login' and approve its device code in Dex the way a browser
would (submit the code, then the static password login), so the whole OIDC
device flow can be checked without a person.

    python3 -I .claude/skills/forge/scripts/approve-device-login.py bin/forge [--context NAME]

DEX_LOGIN / DEX_PASSWORD override admin@example.com / password. Only works
against a Dex with the password connector and skipApprovalScreen: true.
Exit status is the CLI's.
"""
import html
import http.cookiejar
import os
import re
import subprocess
import sys
import urllib.parse
import urllib.request

if len(sys.argv) < 2:
    sys.exit(__doc__)
cli = subprocess.Popen([*sys.argv[1:2], "auth", "login", *sys.argv[2:]],
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
url = None
for line in cli.stderr:
    sys.stdout.write("cli| " + line)
    if m := re.search(r"Or open (\S+)", line):
        url = m.group(1)
    if "Waiting for approval" in line:
        break
if not url:
    cli.kill()
    sys.exit("forge auth login printed no verification URL")

browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
resp = browser.open(url)
body = resp.read().decode()
print(f"dex| {resp.status} {resp.geturl()}")
# Dex shows a code form, then the password form; submit each as a browser would.
for _ in range(3):
    form = re.search(r'<form[^>]*action="([^"]*)"[^>]*>(.*?)</form>', body, re.S)
    if not form:
        break
    action = urllib.parse.urljoin(resp.geturl(), html.unescape(form.group(1)))
    names = re.findall(r'<input[^>]*name="([^"]+)"', form.group(2))
    fields = dict(re.findall(r'<input[^>]*name="([^"]+)"[^>]*value="([^"]*)"', form.group(2)))
    if "password" in names:
        fields["login"] = os.environ.get("DEX_LOGIN", "admin@example.com")
        fields["password"] = os.environ.get("DEX_PASSWORD", "password")
    resp = browser.open(action, urllib.parse.urlencode(fields).encode())
    body = resp.read().decode()
    print(f"dex| POST {action} -> {resp.status} {resp.geturl()}")
    if "password" in names:
        break
print("dex|", re.sub(r"\s+", " ", re.sub(r"<[^>]+>", " ", body)).strip()[:120])

out, err = cli.communicate(timeout=60)
sys.stdout.write("cli| " + err + out)
sys.exit(cli.returncode)
