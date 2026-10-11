"""Walks the dashboard's login against a real Dex and forge-api, as a browser on http://localhost:3000 would."""
import base64, hashlib, html, http.cookiejar, json, os, re, sys, urllib.error, urllib.parse, urllib.request

# Usage: web-login-check.py [api-url] [origin]. Needs a Dex with the password
# connector (admin@example.com / password), the public client forge-web and
# the origin in web.allowedOrigins: the local one (make dex, make api).
API = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080"
ORIGIN = sys.argv[2] if len(sys.argv) > 2 else "http://localhost:3000"
ok = True
def check(what, cond, detail=""):
    global ok
    ok = ok and bool(cond)
    print(("ok   " if cond else "FAIL ") + what + (f"  [{detail}]" if detail and not cond else ""))

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None
def call(url, data=None, headers=None, method=None, opener=None):
    req = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    try:
        r = (opener or urllib.request.build_opener(NoRedirect)).open(req)
    except urllib.error.HTTPError as e:
        r = e
    return r.status if hasattr(r, "status") else r.code, r.headers, r.read()

# 1. Discovery from the API, across origins.
st, h, body = call(API + "/forge.v1.AuthService/GetServerInfo", b"{}", {"Content-Type": "application/json", "Origin": ORIGIN})
oidc = json.loads(body)["oidc"]
check("GetServerInfo names the dashboard's client", oidc.get("webClientId") == "forge-web", oidc)
check("its scopes have no offline_access", oidc.get("webScopes") == ["openid", "email", "profile", "groups"], oidc.get("webScopes"))
check("the API allows the dashboard's origin", h.get("Access-Control-Allow-Origin") == ORIGIN, dict(h))

# 2. Dex's discovery document, across origins.
st, h, body = call(oidc["issuer"] + "/.well-known/openid-configuration", headers={"Origin": ORIGIN})
doc = json.loads(body)
check("Dex allows the dashboard's origin on discovery", h.get("Access-Control-Allow-Origin") in (ORIGIN, "*"), dict(h))

# 3. The authorization code flow with PKCE, as the public client.
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
verifier, state = b64(os.urandom(32)), b64(os.urandom(32))
q = {"response_type": "code", "client_id": "forge-web", "redirect_uri": ORIGIN + "/auth/callback", "scope": " ".join(oidc["webScopes"]),
     "state": state, "code_challenge": b64(hashlib.sha256(verifier.encode()).digest()), "code_challenge_method": "S256"}
class StopAtCallback(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        if newurl.startswith(ORIGIN): raise urllib.error.HTTPError(newurl, code, "callback", headers, fp)
        return super().redirect_request(req, fp, code, msg, headers, newurl)
browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), StopAtCallback)
def login():
    callback, page = None, ''
    try:
        resp = browser.open(doc["authorization_endpoint"] + "?" + urllib.parse.urlencode(q))
        page = resp.read().decode()
        for _ in range(3):
            form = re.search(r'<form[^>]*action="([^"]*)"[^>]*>(.*?)</form>', page, re.S)
            if not form: break
            action = urllib.parse.urljoin(resp.geturl(), html.unescape(form.group(1)))
            fields = dict(re.findall(r'<input[^>]*name="([^"]+)"[^>]*value="([^"]*)"', form.group(2)))
            fields.update(login="admin@example.com", password="password")
            resp = browser.open(action, urllib.parse.urlencode(fields).encode())
            page = resp.read().decode()
    except urllib.error.HTTPError as e:
        callback = e.url
    return callback, page
callback, page = login()
back = urllib.parse.parse_qs(urllib.parse.urlparse(callback or "").query)
check("Dex sends the browser back to /auth/callback with a code", callback and "code" in back and back.get("state") == [state], callback or page[:200])
if not callback: sys.exit(1)

form = {"grant_type": "authorization_code", "code": back["code"][0], "redirect_uri": ORIGIN + "/auth/callback", "client_id": "forge-web"}
st, h, body = call(doc["token_endpoint"], urllib.parse.urlencode({**form, "code_verifier": "wrong-" + verifier}).encode(), {"Origin": ORIGIN})
check("Dex refuses the code with a wrong verifier", st >= 400, st)
# A refused exchange spends the code: log in again for the real one.
callback, page = login()
back = urllib.parse.parse_qs(urllib.parse.urlparse(callback or "").query)
form["code"] = back["code"][0]
st, h, body = call(doc["token_endpoint"], urllib.parse.urlencode({**form, "code_verifier": verifier}).encode(), {"Origin": ORIGIN})
tok = json.loads(body) if st == 200 else {}
check("Dex gives an ID token for the code and verifier, with no client secret", "id_token" in tok, body[:200])
check("Dex allows the dashboard's origin on the token endpoint", h.get("Access-Control-Allow-Origin") in (ORIGIN, "*"), dict(h))
check("no refresh token is issued", "refresh_token" not in tok, list(tok))
claims = json.loads(base64.urlsafe_b64decode(tok["id_token"].split(".")[1] + "=="))
check("the token is issued to forge-web", claims.get("aud") == "forge-web", claims.get("aud"))

# 4. The API: preflight, then a call with the dashboard's token.
st, h, _ = call(API + "/forge.v1.AuthService/GetStatus", method="OPTIONS", headers={"Origin": ORIGIN, "Access-Control-Request-Method": "POST",
                "Access-Control-Request-Headers": "authorization,connect-protocol-version,content-type"})
check("the API answers the preflight", st == 204 and h.get("Access-Control-Allow-Origin") == ORIGIN, (st, dict(h)))
hdr = {"Content-Type": "application/json", "Connect-Protocol-Version": "1", "Origin": ORIGIN, "Authorization": "Bearer " + tok["id_token"]}
st, h, body = call(API + "/forge.v1.AuthService/GetStatus", b"{}", hdr)
check("the API accepts the dashboard's token", st == 200 and json.loads(body).get("email") == "admin@example.com", (st, body[:200]))
st, h, body = call(API + "/forge.v1.ServiceCatalogService/ListServices", b"{}", hdr)
check("and lists services with it", st == 200 and h.get("Access-Control-Allow-Origin") == ORIGIN, (st, body[:200]))
st, h, _ = call(API + "/forge.v1.AuthService/GetStatus", method="OPTIONS", headers={"Origin": "http://evil.example.com", "Access-Control-Request-Method": "POST"})
check("another origin's preflight is refused", st == 403 and not h.get("Access-Control-Allow-Origin"), st)
sys.exit(0 if ok else 1)
