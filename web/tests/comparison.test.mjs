import test from 'node:test';
import assert from 'node:assert/strict';
import { comparisonQueries } from '../app/comparison.ts';

for (const kind of ['city', 'zip']) {
  test(`${kind} comparisons receive identical shared property filters`, () => {
    const common = {
      min_price: '200000', max_price: '900000', min_sqft: '1500', max_sqft: '3000',
      beds: '3', baths: '2.5', min_year: '1980', max_year: '2020',
      type: 'Single Family', min_lot: '4000', max_lot: '12000',
    };
    const input = new URLSearchParams({ kind, areas: ' First , Second, First ', ...common });
    const before = input.toString();
    const result = comparisonQueries(input);
    assert.equal(input.toString(), before);
    assert.equal(result.areas.length, 2);
    for (const area of result.areas) {
      const params = new URLSearchParams(area.query);
      assert.equal(params.get(kind), area.name);
      params.delete(kind);
      assert.deepEqual(Object.fromEntries(params), common);
    }
  });
}
test('empty comparisons are rejected', () => {
  assert.throws(() => comparisonQueries(new URLSearchParams({ areas: ', , ' })), /at least one/);
});
