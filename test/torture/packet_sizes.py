#!/usr/bin/env python3
"""Record only TCP/IP packet lengths for torture bulk traffic on loopback.

Run in the same network namespace as torture.mjs, passing an output JSON path.
SIGTERM ends capture; a 90s limit prevents an abandoned diagnostic from running.
No payloads or capability URLs are retained.
"""
import socket,struct,time,json,signal,sys
running=True
signal.signal(signal.SIGTERM,lambda *_:globals().update(running=False))
s=socket.socket(socket.AF_PACKET,socket.SOCK_RAW,socket.htons(3));s.bind(('lo',0));s.settimeout(.25)
lengths={};deadline=time.monotonic()+90
while running and time.monotonic()<deadline:
 try:p=s.recv(131072)
 except TimeoutError:continue
 if len(p)<54 or p[12:14]!=b'\x08\x00' or p[23]!=6:continue
 ihl=(p[14]&15)*4;ports=struct.unpack_from('!HH',p,14+ihl)
 if 18081 not in ports:continue
 n=struct.unpack_from('!H',p,16)[0];lengths[n]=lengths.get(n,0)+1
json.dump({'tcp_ip_length_max':max(lengths,default=0),'packets':sum(lengths.values()),'above_mtu':sum(c for n,c in lengths.items() if n>1500),'lengths':lengths},open(sys.argv[1],'w'))
