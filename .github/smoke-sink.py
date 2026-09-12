# A stand-in for the backio-agent sidecar, so the smoke test can assert what notifio sends it
# without a real backio behind it.
from http.server import BaseHTTPRequestHandler, HTTPServer
import re


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        fields = sorted(set(re.findall(rb'name="([a-z]+)"', body)))
        print(
            "AUTH=%s FIELDS=%s BYTES=%d"
            % (
                self.headers.get("Authorization"),
                ",".join(f.decode() for f in fields),
                len(body),
            ),
            flush=True,
        )
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"ok"}')

    def log_message(self, *args):
        pass


HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
