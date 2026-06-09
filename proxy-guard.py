#!/usr/bin/env python3
"""
Proxy Guard: 健康检查代理网关
- 监听本地端口，转发流量到上游 Clash 代理
- 每次请求前先检查 Clash 代理是否存活
- 如果代理挂了，直接拒绝请求，防止直连暴露
"""
import http.server
import socketserver
import urllib.parse
import urllib.request
import socket
import time
import json
import threading
import sys

UPSTREAM_PROXY = "http://127.0.0.1:7890"
CLASH_API = "http://127.0.0.1:9090"
LISTEN_PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 7891
HEALTH_TIMEOUT = 2
REQUEST_TIMEOUT = 60

# 全局状态
clash_healthy = True
clash_last_check = 0
CHECK_INTERVAL = 10  # 每10秒检查一次

def check_clash_health():
    """检查 Clash 代理是否可用"""
    global clash_healthy, clash_last_check
    try:
        # 方法1: 检查 Clash API
        req = urllib.request.Request(f"{CLASH_API}/version")
        with urllib.request.urlopen(req, timeout=HEALTH_TIMEOUT) as resp:
            if resp.status == 200:
                clash_healthy = True
                return True
    except:
        pass

    try:
        # 方法2: 尝试连接代理端口
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(HEALTH_TIMEOUT)
        result = sock.connect_ex(("127.0.0.1", 7890))
        sock.close()
        clash_healthy = (result == 0)
        return clash_healthy
    except:
        clash_healthy = False
        return False

class ProxyGuardHandler(http.server.BaseHTTPRequestHandler):
    # 减少日志
    def log_message(self, format, *args):
        if self.path.startswith("/health"):
            return
        # 只记录关键信息
        sys.stderr.write(f"[ProxyGuard] {self.command} {self.path} -> {args}\n")

    def do_CONNECT(self):
        """处理 HTTPS CONNECT 请求"""
        global clash_healthy, clash_last_check

        now = time.time()
        if now - clash_last_check > CHECK_INTERVAL:
            check_clash_health()
            clash_last_check = now

        if not clash_healthy:
            self.send_error(503, "Proxy Unavailable: Upstream proxy is down")
            return

        # 转发到上游代理
        try:
            # 解析目标
            target = self.path
            # 连接到上游代理
            proxy_url = urllib.parse.urlparse(UPSTREAM_PROXY)
            upstream_conn = socket.create_connection(
                (proxy_url.hostname, proxy_url.port), timeout=10
            )
            # 发送 CONNECT 到上游
            upstream_conn.sendall(f"CONNECT {target} HTTP/1.1\r\nHost: {target}\r\n\r\n")
            response = upstream_conn.recv(4096).decode()
            status_line = response.split("\r\n")[0]

            if "200" in status_line:
                # 连接成功，向客户端返回 200
                self.send_response(200, "Connection established")
                self.end_headers()

                # 桥接数据
                self._bridge(upstream_conn)
            else:
                self.send_error(502, f"Upstream proxy error: {status_line}")
                upstream_conn.close()
        except Exception as e:
            self.send_error(502, f"Proxy error: {str(e)}")

    def _bridge(self, upstream_conn):
        """桥接客户端和上游连接"""
        import select
        client = self.connection
        client.setblocking(False)
        upstream_conn.setblocking(False)

        while True:
            try:
                readable, _, exceptional = select.select(
                    [client, upstream_conn], [], [client, upstream_conn], 30
                )
                if exceptional:
                    break
                for sock in readable:
                    other = upstream_conn if sock is client else client
                    try:
                        data = sock.recv(65536)
                        if not data:
                            return
                        other.sendall(data)
                    except:
                        return
            except:
                break

    def do_GET(self):
        self._forward_request()

    def do_POST(self):
        self._forward_request()

    def do_PUT(self):
        self._forward_request()

    def do_DELETE(self):
        self._forward_request()

    def _forward_request(self):
        global clash_healthy, clash_last_check

        now = time.time()
        if now - clash_last_check > CHECK_INTERVAL:
            check_clash_health()
            clash_last_check = now

        if not clash_healthy:
            self.send_error(503, "Proxy Unavailable: Upstream proxy is down")
            return

        # 转发 HTTP 请求到上游代理
        try:
            proxy_url = urllib.parse.urlparse(UPSTREAM_PROXY)
            upstream_conn = socket.create_connection(
                (proxy_url.hostname, proxy_url.port), timeout=10
            )
            # 转发原始请求
            content_length = self.headers.get("Content-Length")
            request_line = f"{self.command} {self.path} HTTP/1.1\r\n"
            headers = ""
            for key, value in self.headers.items():
                headers += f"{key}: {value}\r\n"
            headers += "\r\n"

            upstream_conn.sendall(request_line.encode())
            upstream_conn.sendall(headers.encode())

            if content_length:
                body = self.rfile.read(int(content_length))
                upstream_conn.sendall(body)

            # 读取上游响应
            response = b""
            upstream_conn.settimeout(REQUEST_TIMEOUT)
            while True:
                try:
                    chunk = upstream_conn.recv(65536)
                    if not chunk:
                        break
                    response += chunk
                except socket.timeout:
                    break

            if response:
                # 解析状态行
                parts = response.split(b"\r\n", 1)
                status_line = parts[0].decode()
                status_code = int(status_line.split(" ")[1])

                self.send_response_only(status_line)
                if len(parts) > 1:
                    header_data = parts[1]
                    header_end = header_data.find(b"\r\n\r\n")
                    if header_end >= 0:
                        headers_raw = header_data[:header_end].decode()
                        body = header_data[header_end+4:]
                        for h in headers_raw.split("\r\n"):
                            if h and ":" in h:
                                k, v = h.split(":", 1)
                                self.send_header(k.strip(), v.strip())
                        self.end_headers()
                        if body:
                            self.wfile.write(body)
            upstream_conn.close()
        except Exception as e:
            self.send_error(502, f"Proxy error: {str(e)}")

class ThreadedHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

if __name__ == "__main__":
    # 启动时检查
    check_clash_health()
    clash_last_check = time.time()

    status = "✅ 可用" if clash_healthy else "❌ 不可用"
    print(f"[ProxyGuard] 启动在端口 {LISTEN_PORT}, Clash 状态: {status}", flush=True)

    server = ThreadedHTTPServer(("127.0.0.1", LISTEN_PORT), ProxyGuardHandler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        server.shutdown()
