"""Local Account Guard API credential bridge; Unix socket only, no TCP listener."""
import concurrent.futures, hmac, http.server, json, os, pathlib, re, socketserver, subprocess, urllib.request, urllib.error
ROOT=pathlib.Path(os.environ.get('MODEL_GUARD_DATA_DIR','/root/sub2api-deploy/data/sub2api-account-guard'))
DB_CONTAINER=os.environ.get('MODEL_GUARD_DB_CONTAINER','sub2api-postgres')
ADMIN_URL=os.environ.get('MODEL_GUARD_ADMIN_URL','http://127.0.0.1:8080').rstrip('/')
RUN_UID=int(os.environ.get('MODEL_GUARD_UID','1000'))
RUN_GID=int(os.environ.get('MODEL_GUARD_GID','1000'))
SOCKET=ROOT/'api-bridge.sock';TOKEN=(ROOT/'api-bridge.token').read_text().strip()
class Handler(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def reply(self,status,data):
  raw=json.dumps(data).encode();self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
 def do_GET(self):
  if not hmac.compare_digest(self.headers.get('x-bridge-token',''),TOKEN):return self.reply(403,{'error':'bridge authorization required'})
  m=re.fullmatch(r'/accounts/([1-9][0-9]{0,17})',self.path)
  if not m:return self.reply(404,{'error':'not found'})
  aid=int(m[1]);key=self.headers.get('x-admin-key','')
  if not key:return self.reply(403,{'error':'account scope required'})
  try:
   req=urllib.request.Request(ADMIN_URL+'/api/v1/admin/accounts/'+str(aid),headers={'x-api-key':key})
   with urllib.request.urlopen(req,timeout=10) as r:allowed=json.load(r)['data']
   if allowed.get('id')!=aid or allowed.get('platform')!='openai' or allowed.get('type')!='apikey':return self.reply(403,{'error':'account outside supported scope'})
   # All interpolation is a validated positive integer. SELECT only; never mutate accounts.
   sql="""select json_build_object('id',a.id,'name',a.name,'platform',a.platform,'type',a.type,'status',a.status,'schedulable',a.schedulable,'updated_at',a.updated_at,'expires_at',a.expires_at,'parent_account_id',a.parent_account_id,'credentials',a.credentials,'proxy',case when p.id is null then null else json_build_object('protocol',p.protocol,'host',p.host,'port',p.port,'username',p.username,'password',p.password) end) from accounts a left join proxies p on p.id=a.proxy_id where a.id=%d and a.deleted_at is null and a.platform='openai' and a.type='apikey'"""%aid
   out=subprocess.run(['docker','exec',DB_CONTAINER,'psql','-U','sub2api','-d','sub2api','-Atc',sql],capture_output=True,timeout=12,check=True).stdout
   if not out.strip():return self.reply(404,{'error':'account not found'})
   account=json.loads(out)
   if account['status']!=allowed['status'] or account['schedulable']!=allowed['schedulable']:return self.reply(409,{'error':'account changed; retry'})
   # Recheck scope after reading the secret snapshot, without exposing it on failure.
   with urllib.request.urlopen(req,timeout=10) as r:latest=json.load(r)['data']
   if latest.get('id')!=aid or latest.get('type')!='apikey' or latest.get('status')!=account['status'] or latest.get('schedulable')!=account['schedulable']:return self.reply(409,{'error':'account scope changed'})
   return self.reply(200,account)
  except urllib.error.HTTPError as e:return self.reply(e.code,{'error':'management authorization or account lookup failed'})
  except Exception:return self.reply(503,{'error':'credential bridge temporarily unavailable'})
class Server(socketserver.UnixStreamServer):
 def __init__(self,*a,**k):self.pool=concurrent.futures.ThreadPoolExecutor(max_workers=8);super().__init__(*a,**k)
 def process_request(self,request,address):self.pool.submit(self.process,request,address)
 def process(self,request,address):
  try:request.settimeout(40);self.finish_request(request,address)
  except Exception:pass
  finally:self.shutdown_request(request)
if SOCKET.exists():SOCKET.unlink()
with Server(str(SOCKET),Handler) as server:
 os.chown(SOCKET,RUN_UID,RUN_GID);os.chmod(SOCKET,0o600);server.serve_forever()
