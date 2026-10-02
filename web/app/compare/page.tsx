'use client';

import { useState } from 'react';
import { get, Metrics, metricFields, metricValue } from '../lib';
import Filters, { filterParams } from '../components/Filters';
import Coverage from '../components/Coverage';
import { comparisonQueries } from '../comparison';

export default function Compare() {
  const [results, setResults] = useState<{ name: string; metrics: Metrics }[]>([]);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [applied, setApplied] = useState('');
  return <>
    <p className="eyebrow">SIDE BY SIDE</p><h1>Compare markets</h1>
    <p>Objective measurements from last-observed listing history, with identical property filters for every area.</p>
    <Coverage />
    <form onSubmit={async event => {
      event.preventDefault();
      const params = filterParams(event.currentTarget);
      setBusy(true); setError(''); setResults([]);
      try {
        const { areas, shared } = comparisonQueries(params);
        const values = await Promise.all(areas.map(async area => ({
          name: area.name, metrics: await get<Metrics>('analytics?' + area.query),
        })));
        setResults(values);
        setApplied(shared.size ? [...shared].map(([key, value]) => `${key}: ${value}`).join(' · ') : 'All property types and ranges');
      } catch (err) { setError(String(err)); } finally { setBusy(false); }
    }}>
      <label>Compare by<select name="kind"><option value="zip">ZIP code</option><option value="city">City</option></select></label>
      <label>Areas, separated by commas<input name="areas" required placeholder="75001, 75080" /></label>
      <h2>Shared property filters</h2><Filters />
      <button disabled={busy}>{busy ? 'Loading...' : 'Compare'}</button>
      <button type="reset" className="secondary" disabled={busy} onClick={() => { setResults([]); setError(''); }}>Reset</button>
    </form>
    {error && <p role="alert" className="error">{error}</p>}
    {results.length > 0 && <section className="panel table-wrap">
      <p className="muted">Applied equally to every area: {applied}</p>
      <table><thead><tr><th>Measurement</th>{results.map(result => <th key={result.name}>{result.name}</th>)}</tr></thead>
        <tbody>{[...metricFields, ['became_inactive', 'Listings becoming inactive'], ['became_active', 'Listings becoming active again'], ['average_dom', 'Average DOM'], ['median_reduction', 'Median reduction'], ['median_days_to_inactive', 'Median observed days to inactive']].map(([key, label]) =>
          <tr key={key}><th>{label}</th>{results.map(result => <td key={result.name}>{metricValue(key, result.metrics[key])}</td>)}</tr>)}</tbody>
      </table>
    </section>}
    <p className="muted">Transition counts cover collected history and count distinct listings. Inactivity does not establish a contract or sale. Coverage warnings apply globally, not only to the selected areas.</p>
  </>;
}
