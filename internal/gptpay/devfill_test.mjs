import test from 'node:test';
import assert from 'node:assert/strict';
import { TAXFREE_FIXTURES } from './web/devfill.mjs';

test('fixtures cover all five US sales-tax-free states', () => {
  const states = new Set(TAXFREE_FIXTURES.map(f => f.state));
  assert.deepEqual([...states].sort(), ['AK', 'DE', 'MT', 'NH', 'OR']);
});

test('every fixture has a complete 5-digit ZIP, 2-letter state, and non-empty street / city', () => {
  for (const f of TAXFREE_FIXTURES) {
    assert.match(f.zip, /^\d{5}$/, `${f.state} zip is 5 digits`);
    assert.equal(f.state.length, 2, `${f.state} is a 2-letter code`);
    assert.ok(f.street.length > 0, `${f.state} has a street`);
    assert.ok(f.city.length > 0, `${f.state} has a city`);
  }
});

test('fixtures have no duplicates', () => {
  const seen = new Set();
  for (const f of TAXFREE_FIXTURES) {
    const key = `${f.street}|${f.city}|${f.state}|${f.zip}`;
    assert.ok(!seen.has(key), `duplicate fixture: ${key}`);
    seen.add(key);
  }
});
