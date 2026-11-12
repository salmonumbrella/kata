#!/usr/bin/env python3
"""Proof-only loopback AgentsView usage producer with synthetic sessions.

Serves GET /api/v1/sessions/<id>/usage from a JSON map of session ID to the
producer's usage document. Unknown IDs get AgentsView's session_not_found
answer. Each request first sleeps for the seconds in the delay file, so a
capture can hold a read open long enough to show it pending.

Usage: fake_agentsview.py SESSIONS_JSON DELAY_FILE PORT_FILE
"""

import http.server
import json
import pathlib
import sys
import time
import urllib.parse

sessions = json.loads(pathlib.Path(sys.argv[1]).read_text())
delay_file = pathlib.Path(sys.argv[2])
port_file = pathlib.Path(sys.argv[3])


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        prefix, suffix = "/api/v1/sessions/", "/usage"
        session_id = None
        if path.startswith(prefix) and path.endswith(suffix):
            session_id = urllib.parse.unquote(path[len(prefix) : -len(suffix)])
        try:
            delay = float(delay_file.read_text().strip() or 0)
        except (OSError, ValueError):
            delay = 0
        time.sleep(delay)
        status, body = 200, sessions.get(session_id)
        if body is None:
            status = 404
            body = {"error": {"code": "session_not_found", "message": "session not found"}}
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        sys.stderr.write("fake-agentsview: %s delay=%s\n" % (fmt % args, delay_file.read_text().strip() if delay_file.exists() else "0"))
        sys.stderr.flush()


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port_file.write_text(str(server.server_address[1]))
server.serve_forever()
