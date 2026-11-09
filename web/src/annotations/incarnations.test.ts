import {afterEach,expect,it,vi} from 'vitest';
import {HostAnnotations} from './host';
import {ViewerAnnotations} from './viewer';
import {makeHost,join,viewer} from '../../../e2ee/test/harness';
import {randomBytes} from '../../../e2ee/src/bytes';
import {MsgType,peekType} from '../../../e2ee/src/wire';

afterEach(()=>vi.restoreAllMocks());

it('authentic delayed first delivery from a still-connected prior grant admission cannot erase newer-incarnation accepted ink',async()=>{
 const c=await makeHost();const sid=randomBytes(16);const a=await join(c,'old-peer',viewer(c,{viewerSid:sid}));
 for(const ack of a.acks)await c.host.receive('old-peer',ack);
 const b=await join(c,'new-peer',viewer(c,{viewerSid:sid,grant:a.grant}));for(const ack of b.acks)await c.host.receive('new-peer',ack);
 const ob=await join(c,'observer',viewer(c));for(const ack of ob.acks)await c.host.receive('observer',ack);
 expect(b.rowCode).toBe(a.rowCode);expect(c.host.annotationAllowed(c.idOf('old-peer'))).toBe(true);
 let t=0;const upA:Uint8Array[]=[],upB:Uint8Array[]=[];
 const random=vi.spyOn(Math,'random').mockReturnValue(0);
 const va=new ViewerAnnotations({seal:x=>a.session.sealAnnotation(x),send:x=>upA.push(x),rowCode:()=>a.rowCode},{now:()=>t});
 random.mockReturnValue(0.5);
 const vb=new ViewerAnnotations({seal:x=>b.session.sealAnnotation(x),send:x=>upB.push(x),rowCode:()=>b.rowCode},{now:()=>t});
 const vo=new ViewerAnnotations({seal:async x=>x,send:()=>{},rowCode:()=>ob.rowCode},{now:()=>t});
 const peers=[['old-peer',a,va],['new-peer',b,vb],['observer',ob,vo]] as const;
 const h=new HostAnnotations({onAnnotation:()=>()=>{},onAdmitted:()=>()=>{},onRevoked:()=>()=>{},allowed:id=>c.host.annotationAllowed(id),recipients:()=>peers.map(([p,x])=>({admissionId:c.idOf(p),rowCode:x.rowCode})),sendAnnotations:(id,rs)=>c.host.sendAnnotations(id,rs)},{now:()=>t});
 const off=c.host.on('annotation',e=>h.receive(e));
 const tick=async()=>{t+=1000;await h.flush();for(const[p,x,v]of peers)for(const bytes of c.take(p))if(peekType(bytes)===MsgType.Annotations)v.receive((await x.session.openAnnotations(bytes)).records);};
 const draw=async(v:ViewerAnnotations,up:Uint8Array[])=>{v.setTool('pen');v.input.begin({x:.1,y:.2});v.input.end();v.input.flush(true);await vi.waitFor(()=>expect(up).toHaveLength(1));};
 const ids=(x:{store:typeof h.store})=>[...(x.store.origins.get(a.rowCode)?.strokes.keys()??[])];
 h.setViewers(true);await tick();await draw(va,upA); // hold authentic A's first upstream envelope
 await draw(vb,upB);await c.host.receive('new-peer',upB[0]!);await tick();
 const fresh=ids(vb);expect(ids(h)).toEqual(fresh);expect(ids(vo)).toEqual(fresh);
 await c.host.receive('old-peer',upA[0]!);await tick();for(let i=0;i<40;i++)await tick();
 const result={scenario:'old-grant-incarnation-first-delivery',fresh,old:ids(va),host:ids(h),drawerB:ids(vb),observer:ids(vo),accepted:h.counters.accepted,oldStillAllowed:c.host.annotationAllowed(c.idOf('old-peer'))};console.log(JSON.stringify(result));
 off();for(const[,x,v]of peers){x.session.dispose();v.dispose();}
 expect(result.host).toContain(fresh[0]);expect(result.observer).toContain(fresh[0]);
});
