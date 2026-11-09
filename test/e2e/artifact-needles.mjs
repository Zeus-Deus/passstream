// Sample content beyond common file headers, aligned to base64 triplets so
// encoded slices are also exact substrings of each local transport chunk.
export function artifactNeedles(data, previous = null) {
  const bytes = Buffer.from(data);
  if (bytes.length < 126) throw Error("artifact is too short for a content needle");
  const starts = [96, Math.floor(bytes.length / 2), bytes.length - 126];
  if (previous) {
    let i = 96;
    while (i < Math.min(bytes.length, previous.length) && bytes[i] === previous[i]) i++;
    if (i < bytes.length) starts.push(i);
  }
  return [...new Set(starts.map((s) => Math.floor(Math.max(0, Math.min(s, bytes.length - 126)) / 3) * 3))].map((s) => bytes.subarray(s, s + 126));
}
