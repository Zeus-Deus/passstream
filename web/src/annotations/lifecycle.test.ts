import {afterEach,expect,it,vi} from 'vitest';
import {HostAnnotations,type HostLink} from './host';
import {ViewerAnnotations} from './viewer';
import {decodeAnnotation,encodeAnnotation} from './codec';
import {makeHost,join,viewer} from '../../../e2ee/test/harness';
import {randomBytes} from '../../../e2ee/src/bytes';
import {MsgType,peekType} from '../../../e2ee/src/wire';
const all:ViewerAnnotations[]=[];
afterEach(()=>{all.splice(0).forEach(d=>d.dispose());vi.restoreAllMocks();});
const ids=(v:{store:ViewerAnnotations['store']},row:string)=>[...(v.store.origins.get(row)?.strokes.keys()??[])];
const settle=async()=>{for(let i=0;i<40;i++)await Promise.resolve();};

it.each(['none','global','origin'] as const)('real grant re-admission after sender reload (%s prior barrier) restores drawing',async scope=>{
 vi.spyOn(Math,'random').mockReturnValue(0);let c=await makeHost();const sid=randomBytes(16);let a=await join(c,'drawer',viewer(c,{viewerSid:sid}));
 for(const ack of a.acks) await c.host.receive('drawer',ack);
 let t=0;const pending:Promise<void>[]=[];const sent:Uint8Array[]=[];
 const drawer=new ViewerAnnotations({seal:b=>a.session.sealAnnotation(b),send:b=>{sent.push(b);pending.push(c.host.receive('drawer',b));},rowCode:()=>a.rowCode},{now:()=>t});all.push(drawer);
 const router=()=>new HostAnnotations({onAnnotation:()=>()=>{},onAdmitted:()=>()=>{},onRevoked:()=>()=>{},allowed:id=>c.host.annotationAllowed(id),recipients:()=>[{admissionId:c.idOf('drawer'),rowCode:a.rowCode}],sendAnnotations:(id,rs)=>c.host.sendAnnotations(id,rs)},{now:()=>t});
 let h=router();let off=c.host.on('annotation',e=>h.receive(e));
 const down=async()=>{for(const b of c.take('drawer'))if(peekType(b)===MsgType.Annotations)drawer.receive((await a.session.openAnnotations(b)).records);};
 const tick=async()=>{t+=1000;await h.flush();await down();};
 const draw=async()=>{const count=sent.length;drawer.setTool('pen');drawer.input.begin({x:0.2,y:0.3});drawer.input.end();drawer.input.flush(true);await vi.waitFor(()=>expect(sent.length).toBe(count+1));await Promise.all(pending.splice(0));};
 h.setViewers(true);await tick();await draw();await tick();expect(ids(h,a.rowCode)).toHaveLength(1);
 if(scope==='global')h.clear();
 if(scope==='origin')h.revoke({admissionIds:[],rowCode:a.rowCode});
 if(scope!=='none'){await tick();expect(ids(drawer,a.rowCode)).toEqual([]);}
 const before={epoch:c.active().epoch,host:ids(h,a.rowCode),drawer:ids(drawer,a.rowCode)};
 const old=a;old.session.dispose();off();
 c=await makeHost({storage:c.storage,identity:c.identity,linkSecret:c.linkSecret});
 a=await join(c,'drawer',viewer(c,{viewerSid:sid,grant:old.grant}));
 for(const ack of a.acks)await c.host.receive('drawer',ack);
 drawer.beginAdmission();h=router();off=c.host.on('annotation',e=>h.receive(e));
 expect(a.rowCode).toBe(old.rowCode);expect(c.active().epoch).toBeGreaterThan(before.epoch);
 h.setViewers(true);await tick();await draw();await tick();
 for(let i=0;i<40;i++)await tick();
 const result={scenario:'sender-reload',scope,before,currentEpoch:c.active().epoch,host:ids(h,a.rowCode),drawer:ids(drawer,a.rowCode),counters:h.counters};console.log(JSON.stringify(result));
 off();a.session.dispose();
 if(scope==='none') expect(result.host).toEqual(result.drawer.slice(-1)); else expect(result.host).toEqual(result.drawer);expect(result.host).toHaveLength(1);
});

it('real crypto: same-transport supersession fences an old upstream decrypt that is already authenticated',async()=>{
 const c=await makeHost();const sid=randomBytes(16);const old=await join(c,'same',viewer(c,{viewerSid:sid}));
 for(const ack of old.acks)await c.host.receive('same',ack);
 const body=encodeAnnotation({kind:'input',namespace:1n,session:1n,op:1,clearThrough:0,gen:0,barrier:0,msg:{kind:'stroke',id:1,end:true,points:[{x:.1,y:.1}]}});
 const b=await old.session.sealAnnotation(body);let enter!:()=>void,release!:()=>void;
 const ready=new Promise<void>(r=>enter=r),gate=new Promise<void>(r=>release=r);
 const decrypt=crypto.subtle.decrypt.bind(crypto.subtle);
 vi.spyOn(crypto.subtle,'decrypt').mockImplementationOnce(async(...args)=>{const result=await decrypt(...args);enter();await gate;return result;});
 const received:unknown[]=[];c.host.on('annotation',e=>received.push(e));
 const held=c.host.receive('same',b);await ready;
 const next=await join(c,'same',viewer(c,{viewerSid:sid,grant:old.grant}));
 for(const ack of next.acks)await c.host.receive('same',ack);
 release();await held;expect(received).toEqual([]);
 old.session.dispose();next.session.dispose();
});
