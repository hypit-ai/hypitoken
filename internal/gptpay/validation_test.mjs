import test from 'node:test';
import assert from 'node:assert/strict';
import { validSession, validCard, validExpiry, validate, COUNTRIES, CURRENCIES } from './web/validation.mjs';

test('session checks never echo the token', () => {
  assert.equal(validSession('test_token_not_real_0123456789'), true);
  assert.equal(validSession('{"accessToken":"test_token_not_real_0123456789"}'), true);
  for (const s of ['', '{}', '{bad', '{"accessToken":123}', 'short', '<script>alert(1)</script>']) assert.equal(validSession(s), false);
});
test('card check accepts test fixtures and rejects bad checksum', () => {
  assert.equal(validCard('4242 4242 4242 4242'), true);
  assert.equal(validCard('378282246310005'), true);
  for (const card of ['4242424242424241', '0000000000000000', '4242a424242424242', '', '1234']) assert.equal(validCard(card), false);
});
test('expiry includes current month and rejects expired dates', () => {
  const now = new Date(2026, 8, 15);
  assert.equal(validExpiry('09 / 26', now), true);
  assert.equal(validExpiry('12/30', now), true);
  for (const exp of ['08/26', '00/30', '13/30', '2028', '01/2028', '']) assert.equal(validExpiry(exp, now), false);
});
test('all fields validated without returning submitted values', () => {
  const good = { plan: 'chatgptprolite', country: 'PH', currency: 'PHP', session: 'test_token_not_real_0123456789', 'card-number': '4242424242424242', expiry: '12/30', cvv: '123' };
  assert.deepEqual(validate(good, new Date(2026, 8, 15)), {});
  assert.ok(validate({ ...good, plan: 'chatgptpro5x' }).plan);
  assert.ok(validate({ ...good, country: 'XX' }).country);
  assert.ok(validate({ ...good, currency: 'ZZZ' }).currency);
  assert.ok(validate({ ...good, cvv: '12a' }).cvv);
  assert.ok(validate({ ...good, cvv: '12' }).cvv);
  assert.equal(JSON.stringify(validate({})).includes(good.session), false);
});
test('selection catalogs contain the requested country and currency', () => {
  for (const c of ['US', 'PH', 'GB', 'JP']) assert.ok(COUNTRIES.includes(c));
  for (const c of ['USD', 'PHP', 'GBP', 'JPY']) assert.ok(CURRENCIES.includes(c));
  assert.equal(new Set(COUNTRIES).size, COUNTRIES.length);
  assert.equal(new Set(CURRENCIES).size, CURRENCIES.length);
});
