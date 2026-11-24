"""Mock OpenID Connect provider and auth-proxy token signer, for vitrine's sign-in tests.

OIDC: discovery, /authorize (auto-approves as the user in /tmp/idp_user.json),
/token (checks client secret and PKCE S256), /jwks, RS256 ID tokens.
Proxy: /sign signs an arbitrary claim set with an ES256 key (IAP-style),
published at /jwks-ec; ?key=rogue signs with a key nobody publishes.
"""
import base64, hashlib, json, os, secrets, time, urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import jwt
from cryptography.hazmat.primitives.asymmetric import rsa, ec

ISS = "http://127.0.0.1:8892"
CLIENT, SECRET = "vitrine", "s3cret"
rsa_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
ec_key = ec.generate_private_key(ec.SECP256R1())
rogue = ec.generate_private_key(ec.SECP256R1())
codes = {}

def jwk(pub, kid, alg):
    d = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(pub) if alg == "RS256" else jwt.algorithms.ECAlgorithm.to_jwk(pub))
    d.update(kid=kid, alg=alg, use="sig")
    return d

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, body=b"", ctype="application/json", headers={}):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        for k, v in headers.items(): self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body if isinstance(body, bytes) else json.dumps(body).encode())
    def do_GET(self):
        u = urllib.parse.urlparse(self.path); q = dict(urllib.parse.parse_qsl(u.query))
        if u.path == "/.well-known/openid-configuration":
            return self.send(200, {"issuer": ISS, "authorization_endpoint": ISS + "/authorize", "token_endpoint": ISS + "/token",
                "jwks_uri": ISS + "/jwks", "response_types_supported": ["code"], "subject_types_supported": ["public"],
                "id_token_signing_alg_values_supported": ["RS256"], "code_challenge_methods_supported": ["S256"]})
        if u.path == "/jwks":
            return self.send(200, {"keys": [jwk(rsa_key.public_key(), "r1", "RS256")]})
        if u.path == "/jwks-ec":
            return self.send(200, {"keys": [jwk(ec_key.public_key(), "e1", "ES256")]})
        if u.path == "/authorize":
            if q.get("client_id") != CLIENT or q.get("code_challenge_method") != "S256":
                return self.send(400, {"error": "bad request"})
            user = json.load(open("/tmp/idp_user.json"))
            code = secrets.token_urlsafe(16)
            codes[code] = {"user": user, "nonce": q.get("nonce"), "challenge": q["code_challenge"], "redirect_uri": q["redirect_uri"]}
            loc = q["redirect_uri"] + "?" + urllib.parse.urlencode({"code": code, "state": q["state"]})
            return self.send(302, b"", "text/plain", {"Location": loc})
        if u.path == "/sign":
            claims = json.loads(q["claims"])
            key = rogue if q.get("key") == "rogue" else ec_key
            tok = jwt.encode(claims, key, algorithm="ES256", headers={"kid": "e1"})
            return self.send(200, tok.encode(), "text/plain")
        self.send(404, {"error": "not found"})
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        f = dict(urllib.parse.parse_qsl(self.rfile.read(n).decode()))
        if self.path != "/token":
            return self.send(404, {})
        cid, sec = f.get("client_id"), f.get("client_secret")
        if self.headers.get("Authorization", "").startswith("Basic "):
            cid, sec = [urllib.parse.unquote(x) for x in base64.b64decode(self.headers["Authorization"][6:]).decode().split(":", 1)]
        if (cid, sec) != (CLIENT, SECRET):
            return self.send(401, {"error": "invalid_client"})
        c = codes.pop(f.get("code"), None)
        if not c:
            return self.send(400, {"error": "invalid_grant"})
        chal = base64.urlsafe_b64encode(hashlib.sha256(f.get("code_verifier", "").encode()).digest()).rstrip(b"=").decode()
        if chal != c["challenge"]:
            return self.send(400, {"error": "invalid_grant", "error_description": "pkce"})
        u = c["user"]; now = int(time.time())
        claims = {"iss": ISS, "aud": CLIENT, "sub": u["sub"], "iat": now, "exp": now + 300, "nonce": c["nonce"], **{k: v for k, v in u.items() if k != "sub"}}
        idt = jwt.encode(claims, rsa_key, algorithm="RS256", headers={"kid": "r1"})
        self.send(200, {"access_token": secrets.token_urlsafe(16), "token_type": "Bearer", "expires_in": 300, "id_token": idt})

ThreadingHTTPServer(("127.0.0.1", 8892), H).serve_forever()
