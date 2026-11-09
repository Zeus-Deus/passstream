"""Exercise the actual HTTP connection cap and incomplete-header deadline."""
import json,os,socket,subprocess,tempfile,time,sys
from pathlib import Path
binary=str(Path(sys.argv[1] if len(sys.argv)>1 else 'build/passstream').resolve())
state=tempfile.mkdtemp(prefix='passstream-r5-header-')
p=subprocess.Popen([binary,'serve','--data',state,'--listen','127.0.0.1:20380','--base-url','http://127.0.0.1:20380','--ice-udp','127.0.0.1:20343','--ice-tcp','127.0.0.1:20343','--turn','127.0.0.1:20378','--max-connections','4'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
clients=[]
try:
 for _ in range(200):
  try:
   c=socket.create_connection(('127.0.0.1',20380),.1);c.close();break
  except OSError:time.sleep(.025)
 for _ in range(4):
  c=socket.create_connection(('127.0.0.1',20380));c.sendall(b'GET /healthz HTTP/1.1\r\nHost: localhost\r\nX-Hold: ');clients.append(c)
 time.sleep(.1)
 check=socket.create_connection(('127.0.0.1',20380));check.sendall(b'GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n');check.settimeout(.5)
 held=False
 try:check.recv(4096)
 except socket.timeout:held=True
 clients.pop(0).close();check.settimeout(2);released=b'200 OK' in check.recv(4096);check.close()
 c=clients[0];c.settimeout(12);start=time.monotonic();closed=c.recv(4096)==b'';elapsed=time.monotonic()-start
 result={'binary':binary,'sha256':subprocess.check_output(['sha256sum',binary],text=True).split()[0],'connection_cap':4,'fifth_request_held':held,'released_after_close':released,'incomplete_header_closed':closed,'remaining_header_deadline_s':elapsed,'pass':held and released and closed and 8.5<elapsed<10.5}
 print(json.dumps(result,indent=2));Path('test/torture/results/verification/r5-header-cap.json').write_text(json.dumps(result,indent=2)+'\n')
 if not result['pass']:raise SystemExit(1)
finally:
 for c in clients:c.close()
 p.terminate();p.wait(timeout=10)
