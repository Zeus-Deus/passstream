import {afterEach,expect,it,vi} from 'vitest';
import {ViewerController} from '../viewer/controller';
import {HostAnnotations} from './host';
import {makeHost,join,viewer} from '../../../e2ee/test/harness';
import {fromBase64Url,randomBytes,toBase64Url} from '../../../e2ee/src/bytes';
import {MsgType,peekType} from '../../../e2ee/src/wire';
import {decodeAnnotation} from './codec';
afterEach(()=>vi.restoreAllMocks());

it.each(['none','global','origin'] as const)('actual ViewerController admission path after sender reload, prior %s barrier',async scope=>{
 vi.spyOn(Math,'random').mockReturnValue(0);let c=await makeHost();const sid=randomBytes(16);let a=await join(c,'drawer',viewer(c,{viewerSid:sid}));
 const remembered={...a.grant,secret:a.grant.secret.slice()};
 const v=new ViewerController();v['senderInstance']='page-1';v['connectionId']='drawer';
 const inFlight:Promise<void>[]=[];let sends=0;
 v.session={sendSignal:(msg:{b:string})=>{sends++;inFlight.push(c.host.receive('drawer',fromBase64Url(msg.b)));return true;},stop:()=>{},startMedia:()=>{}} as never;
 v['admittedWith'](a);
 let t=0;let h!:HostAnnotations;
 const router=()=>new HostAnnotations({onAnnotation:()=>()=>{},onAdmitted:()=>()=>{},onRevoked:()=>()=>{},allowed:id=>c.host.annotationAllowed(id),recipients:()=>[{admissionId:c.idOf('drawer'),rowCode:a.rowCode}],sendAnnotations:(id,rs)=>c.host.sendAnnotations(id,rs)},{now:()=>t});
 h=router();let off=c.host.on('annotation',e=>h.receive(e));
 const tick=async()=>{t+=1000;await h.flush();for(const b of c.take('drawer'))if(peekType(b)===MsgType.Annotations)await v['receive'](toBase64Url(b));};
 const draw=async()=>{const count=sends;v.annotations.setTool('pen');v.annotations.input.begin({x:.3,y:.4});v.annotations.input.end();v.annotations.input.flush(true);await vi.waitFor(()=>expect(sends).toBe(count+1));await Promise.all(inFlight.splice(0));};
 const ids=(obj:{store:typeof h.store})=>[...(obj.store.origins.get(a.rowCode)?.strokes.keys()??[])];
 h.setViewers(true);await tick();await draw();await tick();
 if(scope==='global')h.clear();if(scope==='origin')h.revoke({admissionIds:[],rowCode:a.rowCode});await tick();
 off();a.session.dispose();
 c=await makeHost({storage:c.storage,identity:c.identity,linkSecret:c.linkSecret});
 a=await join(c,'drawer',viewer(c,{viewerSid:sid,grant:remembered}));
 // Use the same actual admission completion that follows sender_back; no worker/PC is started.
 v['senderInstance']='page-2';v['admittedWith'](a);
 h=router();off=c.host.on('annotation',e=>h.receive(e));h.setViewers(true);await tick();await draw();await tick();
 for(let n=0;n<40;n++)await tick();
 const result={scenario:'actual-controller-reload',scope,admissions:v.admissions,host:ids(h),drawer:ids(v.annotations),stale:h.counters.dropped_stale,accepted:h.counters.accepted,rejects:v.annotationRejects};
 console.log(JSON.stringify(result));
 off();a.session.dispose();v.annotations.dispose();
 expect(result.admissions).toBe(2);expect(result.rejects).toBe(0);expect(result.host).toHaveLength(1);
 expect(result.host).toEqual(result.drawer.slice(-1));
});

it.each(['none','global','origin'] as const)('actual ViewerController ordinary same-host PC replacement, prior %s barrier',async scope=>{
 vi.spyOn(Math,'random').mockReturnValue(0);let c=await makeHost();const sid=randomBytes(16);let a=await join(c,'drawer',viewer(c,{viewerSid:sid}));
 const remembered={...a.grant,secret:a.grant.secret.slice()};
 const v=new ViewerController();v['senderInstance']='page-1';v['connectionId']='drawer';
 const inFlight:Promise<void>[]=[];let sends=0;
 v.session={sendSignal:(msg:{b:string})=>{sends++;inFlight.push(c.host.receive('drawer',fromBase64Url(msg.b)));return true;},stop:()=>{},startMedia:()=>{}} as never;
 v['admittedWith'](a);
 let t=0;let h!:HostAnnotations;
 const router=()=>new HostAnnotations({onAnnotation:()=>()=>{},onAdmitted:()=>()=>{},onRevoked:()=>()=>{},allowed:id=>c.host.annotationAllowed(id),recipients:()=>[{admissionId:c.idOf('drawer'),rowCode:a.rowCode}],sendAnnotations:(id,rs)=>c.host.sendAnnotations(id,rs)},{now:()=>t});
 h=router();let off=c.host.on('annotation',e=>h.receive(e));
 const tick=async()=>{t+=1000;await h.flush();for(const b of c.take('drawer'))if(peekType(b)===MsgType.Annotations)await v['receive'](toBase64Url(b));};
 const draw=async()=>{const count=sends;v.annotations.setTool('pen');v.annotations.input.begin({x:.3,y:.4});v.annotations.input.end();v.annotations.input.flush(true);await vi.waitFor(()=>expect(sends).toBe(count+1));await Promise.all(inFlight.splice(0));};
 const ids=(obj:{store:typeof h.store})=>[...(obj.store.origins.get(a.rowCode)?.strokes.keys()??[])];
 h.setViewers(true);await tick();await draw();await tick();
 if(scope==='global')h.clear();if(scope==='origin')h.revoke({admissionIds:[],rowCode:a.rowCode});await tick();
 off();a.session.dispose();
 await c.host.peerClosed('drawer');
 a=await join(c,'drawer',viewer(c,{viewerSid:sid,grant:remembered}));
 // Use the same actual admission completion that follows sender_back; no worker/PC is started.
 v['senderInstance']='page-1';v['admittedWith'](a);
 off=c.host.on('annotation',e=>h.receive(e));h.setViewers(true);await tick();await draw();await tick();
 for(let n=0;n<40;n++)await tick();
 const result={scenario:'actual-controller-reload',scope,admissions:v.admissions,host:ids(h),drawer:ids(v.annotations),stale:h.counters.dropped_stale,accepted:h.counters.accepted,rejects:v.annotationRejects};
 console.log(JSON.stringify(result));
 off();a.session.dispose();v.annotations.dispose();
 expect(result.admissions).toBe(2);expect(result.rejects).toBe(0);expect(result.host).toHaveLength(scope==='none'?2:1);
 expect(result.host).toEqual(result.drawer);
});

it.each(['seal','open'] as const)('actual controller fences held %s output when an authenticated admission replaces its channel',async kind=>{
 const c=await makeHost(),sid=randomBytes(16);const a=await join(c,'drawer',viewer(c,{viewerSid:sid}));
 const grant={...a.grant,secret:a.grant.secret.slice()};
 const v=new ViewerController();const sent:Uint8Array[]=[];
 v.session={sendSignal:(msg:{b:string})=>{sent.push(fromBase64Url(msg.b));return true;},stop:()=>{},startMedia:()=>{}} as never;
 v['admittedWith'](a);sent.length=0;
 let t=0;const h=new HostAnnotations({onAnnotation:()=>()=>{},onAdmitted:()=>()=>{},onRevoked:()=>()=>{},allowed:id=>c.host.annotationAllowed(id),recipients:()=>[{admissionId:c.idOf('drawer'),rowCode:a.rowCode}],sendAnnotations:(id,rs,current)=>c.host.sendAnnotations(id,rs,current)},{now:()=>t});
 const tick=async()=>{t+=1000;await h.flush();for(const b of c.take('drawer'))if(peekType(b)===MsgType.Annotations)await v['receive'](toBase64Url(b));};
 h.setViewers(true);await tick();expect(v.annotations.policy).toBe(true);
 let entered!:()=>void,release!:()=>void;const ready=new Promise<void>(r=>entered=r),gate=new Promise<void>(r=>release=r);
 let held:Promise<void>|undefined;
 if(kind==='seal'){
  const seal=a.session.sealAnnotation.bind(a.session);
  vi.spyOn(a.session,'sealAnnotation').mockImplementationOnce(async b=>{const out=await seal(b);entered();await gate;return out;});
  v.annotations.setTool('pen');v.annotations.input.begin({x:.2,y:.3});v.annotations.input.end();v.annotations.input.flush(true);
 }else{
  h.setViewers(false);t+=1000;await h.flush();const packet=c.take('drawer').find(b=>peekType(b)===MsgType.Annotations)!;
  const open=a.session.openAnnotations.bind(a.session);
  vi.spyOn(a.session,'openAnnotations').mockImplementationOnce(async b=>{const out=await open(b);entered();await gate;return out;});
  held=v['receive'](toBase64Url(packet));
 }
 await ready;
 const next=await join(c,'drawer',viewer(c,{viewerSid:sid,grant}));v['admittedWith'](next);sent.length=0;
 h.setViewers(true);await tick();expect(v.annotations.policy).toBe(true);
 release();if(held)await held;await vi.waitFor(()=>expect(v.annotations['pumping']).toBe(false));
 expect(sent).toHaveLength(0);expect(v.annotations.policy).toBe(true);
 v.annotations.setTool('pen');v.annotations.input.begin({x:.4,y:.5});v.annotations.input.end();v.annotations.input.flush(true);
 await vi.waitFor(()=>expect(sent).toHaveLength(1));
 let accepted=0;const off=c.host.on('annotation',e=>{h.receive(e);accepted++;});await c.host.receive('drawer',sent[0]!);expect(accepted).toBe(1);expect(h.counters.accepted).toBe(1);
 off();v.annotations.dispose();next.session.dispose();
});
