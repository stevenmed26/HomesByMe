'use client';

import { useEffect, useState } from 'react';
import Link from 'next/link';
import { get, Metrics, metricFields, metricValue, money, num } from './lib';
import Filters, { filterParams } from './components/Filters';
import Coverage from './components/Coverage';

type Listing = {
  id: number; property_id: number; address_line_1: string; city: string; zip_code: string;
  current_price: number | null; square_feet: number | null; ppsf: number | null;
  bedrooms: number | null; bathrooms: number | null; days_on_market: number | null;
  current_status: string; changed: string; last_seen_at: string; stale: boolean;
};
type Usage = { requests: number; month: string; budget: number };

export default function Dashboard() {
  const [query, setQuery] = useState('');
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState('changed');
  const [direction, setDirection] = useState('desc');
  const [metrics, setMetrics] = useState<Metrics>();
  const [rows, setRows] = useState<Listing[]>([]);
  const [usage, setUsage] = useState<Usage>();
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true; setLoading(true); setError('');
    Promise.all([
      get<Metrics>('analytics?' + query),
      get<Listing[]>(`listings?${query}&sort=${sort}&direction=${direction}&page=${page}`),
      get<Usage>('usage'),
    ]).then(([summary, listings, requests]) => {
      if (alive) { setMetrics(summary); setRows(listings); setUsage(requests); }
    }).catch(err => { if (alive) setError(err.message); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [query, sort, direction, page]);

  return <>
    <div className="title"><div><p className="eyebrow">YOUR MARKET, OVER TIME</p><h1>Market dashboard</h1>
      <p>Track last-observed inventory, price movement, and listing changes.</p></div>
      <div className="usage">RentCast requests <strong>{usage?.requests ?? '—'} / {usage?.budget ?? 50}</strong>
        <small>{usage?.month ?? 'Current month'} · UTC</small></div>
    </div>
    <Coverage />
    <form onSubmit={event => { event.preventDefault(); setQuery(filterParams(event.currentTarget).toString()); setPage(0); }}>
      <h2>Explore your market</h2><Filters location />
      <button>Apply filters</button><button type="reset" className="secondary" onClick={() => { setQuery(''); setPage(0); }}>Reset</button>
    </form>
    {error ? <p role="alert" className="error">{error}</p> : loading ? <p role="status">Loading stored observations...</p> : <>
      <section className="metrics">{metricFields.map(([key, label]) => <article key={key}><span>{label}</span><strong>{metricValue(key, metrics?.[key])}</strong></article>)}</section>
      <p className="muted">Price, DOM, and reduction metrics use last-observed active inventory, including stale listings. Relisting rate uses all matching listings and collected history.</p>
      <section className="panel"><h2>Observed listings</h2><p>Includes active and inactive records. Select a heading to sort.</p>
        <div className="table-wrap"><table><thead><tr>{[
          ['address', 'Address'], ['price', 'Price'], ['sqft', 'Sqft'], ['ppsf', '$/sqft'],
          ['beds', 'Beds'], ['baths', 'Baths'], ['dom', 'DOM'], ['status', 'Status'],
          ['changed', 'Last changed'], ['observed', 'Last observed'],
        ].map(([key, label]) => <th key={key}><button className="sort" onClick={() => {
          setSort(key); setDirection(sort === key && direction === 'asc' ? 'desc' : 'asc'); setPage(0);
        }}>{label}{sort === key ? (direction === 'asc' ? ' ↑' : ' ↓') : ''}</button></th>)}</tr></thead>
          <tbody>{rows.map(row => <tr key={row.id}>
            <td><Link href={'/properties/' + row.property_id}>{row.address_line_1}</Link><small>{row.city} · {row.zip_code}</small></td>
            <td>{money(row.current_price)}</td><td>{num(row.square_feet)}</td><td>{money(row.ppsf)}</td>
            <td>{num(row.bedrooms)}</td><td>{num(row.bathrooms)}</td><td>{num(row.days_on_market)}</td>
            <td><span className={'badge ' + row.current_status.toLowerCase()}>{row.current_status}</span></td>
            <td>{row.changed ? new Date(row.changed).toLocaleString() : '—'}</td>
            <td>{new Date(row.last_seen_at).toLocaleString()}{row.stale && <small className="stale-label">Stale</small>}</td>
          </tr>)}</tbody></table></div>
        {rows.length === 0 && <div className="empty"><h3>No listings yet</h3><p>Configure a search area and complete ingestion, or adjust your filters. No sample data is shown.</p></div>}
        <div className="pagination"><button disabled={page === 0} onClick={() => setPage(page - 1)}>Previous</button><span>Page {page + 1}</span><button disabled={rows.length < 100} onClick={() => setPage(page + 1)}>Next</button></div>
      </section>
    </>}
  </>;
}
