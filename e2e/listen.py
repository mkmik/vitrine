"""TCP listener on 127.0.0.1:8896 that logs every connection: harden_test checks nothing reaches it."""
import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR,1); s.bind(('127.0.0.1',8896)); s.listen(5)
while True:
    c,a=s.accept(); print('CONNECT', a, flush=True); c.close()
