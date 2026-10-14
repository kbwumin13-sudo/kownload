#!/usr/bin/env python3
"""Check an installed Gopeed through its real native host, using local fixtures."""
import argparse,hashlib,json,pathlib,struct,subprocess,sys,time
parser=argparse.ArgumentParser()
parser.add_argument('--host',type=pathlib.Path,default=pathlib.Path.home()/'Library/Application Support/com.gopeed.gopeed/host')
parser.add_argument('--output',type=pathlib.Path,required=True)
parser.add_argument('--expected-version',default='1.9.3-media.4')
parser.add_argument('--expected-app',type=pathlib.Path,default=pathlib.Path('/Applications/Gopeed.app'))
parser.add_argument('--fixture',default='http://127.0.0.1:18794')
args=parser.parse_args();args.output.mkdir(parents=True,exist_ok=True)
def native(method,path,data=None):
 params={'method':method,'path':'/api/v1'+path}
 if data is not None:params['data']=data
 wire=json.dumps({'method':'forward','params':params},ensure_ascii=False).encode()
 run=subprocess.run([str(args.host)],input=struct.pack('=I',len(wire))+wire,capture_output=True,timeout=65)
 if len(run.stdout)<4:raise RuntimeError('No native response')
 length=struct.unpack('=I',run.stdout[:4])[0];outer=json.loads(run.stdout[4:4+length])
 if outer.get('code')!=0:raise RuntimeError(outer.get('message','Native failure'))
 rest=outer.get('data',{})
 if rest.get('code')!=0:raise RuntimeError(rest.get('msg',rest.get('error','REST failure')))
 return rest.get('data')
info=native('GET','/info')
assert info['version']==args.expected_version,info
identity=None
if sys.platform=='darwin':
 pids=subprocess.check_output(['pgrep','-x','Gopeed'],text=True).split()
 assert len(pids)==1,'Ambiguous running Gopeed processes'
 pid=pids[0]
 executable=subprocess.check_output(['ps','-p',pid,'-o','comm='],text=True).strip()
 expected_executable=args.expected_app/'Contents/MacOS/Gopeed'
 assert pathlib.Path(executable)==expected_executable,'Running a different Gopeed bundle: '+executable
 loaded=subprocess.check_output(['lsof','-p',pid,'-n','-P'],text=True)
 cores={line.split()[-1] for line in loaded.splitlines() if 'libgopeed.dylib' in line}
 expected_core=args.expected_app/'Contents/Frameworks/libgopeed.dylib'
 assert cores=={str(expected_core)},'Running core does not belong to the expected app'
 cfg=native('GET','/config').get('protocolConfig',{}).get('media',{})
 paths={key:cfg.get(key,'') for key in ('ytDlpPath','ffmpegPath')}
 paths['jsRuntimePath']=cfg.get('jsRuntimePath','/opt/homebrew/bin/node')
 assert all(path and pathlib.Path(path).is_absolute() and pathlib.Path(path).is_file() for path in paths.values()),'Media tool paths must exist and be absolute'
 identity={'pid':int(pid),'executable':executable,'loadedCore':str(expected_core),'coreSha256':hashlib.sha256(expected_core.read_bytes()).hexdigest(),'mediaTools':paths}
results=[]
for title,source,protocol in [('HLS','/hls/master.m3u8','media'),('DASH','/dash/stream.mpd','media'),('图片','/images/1.jpg','')]:
 req={'url':args.fixture+source}
 if protocol:req.update(protocol=protocol,extra={'title':title,'sourcePage':args.fixture+'/index.html','mediaKind':'video'})
 if protocol:
  inspection=native('POST','/media/inspect',{'req':req})
  assert inspection['supported'] and inspection['formats'],inspection
 task_id=native('POST','/tasks',{'req':req,'opts':{'path':str(args.output.resolve())}})
 deadline=time.monotonic()+60
 while time.monotonic()<deadline:
  task=native('GET','/tasks/'+task_id)
  if task['status']=='error':raise RuntimeError('Installed app task failed: '+title)
  if task['status']=='done':break
  time.sleep(.2)
 else:raise RuntimeError('Installed app task timed out: '+title)
 name=task['meta']['opts'].get('name') or task['meta']['res']['files'][0]['name']
 output=args.output/name
 assert output.is_file() and output.stat().st_size>0
 result={'title':title,'taskId':task_id,'output':str(output),'bytes':output.stat().st_size}
 if protocol:
  probe=subprocess.check_output(['/opt/homebrew/bin/ffprobe','-v','error','-show_entries','stream=codec_type','-of','json',str(output)])
  streams=[s['codec_type'] for s in json.loads(probe)['streams']]
  assert 'audio' in streams and 'video' in streams
  result['streams']=streams
 results.append(result)
assert native('GET','/info')['version']==args.expected_version,'Running app changed during acceptance'
report={'installedInfo':info,'runtimeIdentity':identity,'results':results}
(args.output/'acceptance.json').write_text(json.dumps(report,ensure_ascii=False,indent=2))
print(json.dumps(report,ensure_ascii=False,indent=2))
