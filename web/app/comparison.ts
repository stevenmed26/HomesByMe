// Clone one common filter set for every area. Location is the only difference.
export function comparisonQueries(form: URLSearchParams) {
  const kind = form.get('kind') === 'city' ? 'city' : 'zip';
  const names = [...new Set((form.get('areas') || '').split(',').map(value => value.trim()).filter(Boolean))];
  if (!names.length) throw new Error('Enter at least one city or ZIP code.');
  if (names.length > 10) throw new Error('Compare up to 10 areas at a time.');
  const shared = new URLSearchParams(form);
  for (const key of ['kind', 'areas', 'city', 'zip']) shared.delete(key);
  return {
    shared,
    areas: names.map(name => {
      const params = new URLSearchParams(shared);
      params.set(kind, name);
      return { name, query: params.toString() };
    }),
  };
}
