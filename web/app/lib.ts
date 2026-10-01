export async function get<T>(path: string): Promise<T> { const r = await fetch('/api/'+path); if (!r.ok) throw new Error(await r.text()); return r.json(); }
export const money = (v: unknown) => v == null ? '—' : Number(v).toLocaleString('en-US',{style:'currency',currency:'USD',maximumFractionDigits:0});
export const num = (v: unknown) => v == null ? '—' : Number(v).toLocaleString('en-US',{maximumFractionDigits:1});
export type Metrics = Record<string, number | null>;
export const metricFields = [['active_inventory','Active listings'],['median_price','Median price'],['median_ppsf','Median $/sqft'],['median_dom','Median DOM'],['price_reduction_pct','Price reduction %'],['relisting_pct','Relisting %']] as const;
export const metricValue = (key:string,v:unknown) => key==='median_price'||key==='median_ppsf'||key==='median_reduction' ? money(v) : num(v)+(key.endsWith('_pct') && v!=null?'%':'');
