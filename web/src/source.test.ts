import {expect,it} from "vitest";
import {publisherEncoding} from "./source";
it("production capture has a fixed 4Mbit/s L1T3 default",()=>{
 const track={} as MediaStreamTrack;
 expect(publisherEncoding(track)).toEqual({maxBitrate:4_000_000,scalabilityMode:"L1T3"});
 const encoding=publisherEncoding(track);encoding.maxBitrate=1;
 expect(publisherEncoding(track).maxBitrate).toBe(4_000_000);
});
