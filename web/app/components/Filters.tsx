export const propertyFilters = [
  ['min_price', 'Min price'], ['max_price', 'Max price'],
  ['min_sqft', 'Min sqft'], ['max_sqft', 'Max sqft'],
  ['beds', 'Min beds'], ['baths', 'Min baths'],
  ['min_year', 'Year from'], ['max_year', 'Year to'],
  ['type', 'Property type'],
  ['min_lot', 'Min lot sqft'], ['max_lot', 'Max lot sqft'],
] as const;

export function filterParams(form: HTMLFormElement): URLSearchParams {
  const params = new URLSearchParams();
  new FormData(form).forEach((value, key) => {
    if (String(value).trim()) params.set(key, String(value).trim());
  });
  return params;
}

export default function Filters({ location = false }: { location?: boolean }) {
  const fields = location ? [['city', 'City'], ['zip', 'ZIP code'], ...propertyFilters] : propertyFilters;
  return <div className="filters">
    {fields.map(([key, label]) => <label key={key}>{label}
      <input name={key} type={['city', 'zip', 'type'].includes(key) ? 'text' : 'number'}
        min="0" step="any" placeholder={key === 'type' ? 'Single Family' : 'Any'} />
    </label>)}
  </div>;
}
