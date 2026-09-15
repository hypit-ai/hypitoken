import test from 'node:test';
import assert from 'node:assert/strict';
import { TAXFREE_FIXTURES, decodeSessionClaimsProfile } from './web/devfill.mjs';

// fakeJWT builds a real base64url-encoded three-segment string (no signature
// verification is ever performed on it — this only has to look like a JWT to
// the decoder) so the tests exercise the exact same decode path a pasted
// session goes through, not a stand-in for it.
function fakeJWT(claims) {
  const seg = obj => Buffer.from(JSON.stringify(obj)).toString('base64url');
  return `${seg({ alg: 'RS256' })}.${seg(claims)}.sig`;
}

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

test('decodeSessionClaimsProfile reads the real email/name off a bare accessToken', () => {
  const jwt = fakeJWT({ 'https://api.openai.com/profile': { email: 'real@example.com', name: 'Real Person', email_verified: true } });
  assert.deepEqual(decodeSessionClaimsProfile(jwt), { email: 'real@example.com', name: 'Real Person' });
});

test('decodeSessionClaimsProfile unwraps a JSON-wrapped Session first', () => {
  const jwt = fakeJWT({ 'https://api.openai.com/profile': { email: 'wrapped@example.com' } });
  const session = JSON.stringify({ accessToken: jwt, user: { email: 'unrelated@example.com' } });
  // The wrapper's own "user.email" must never be trusted — only the token's
  // own signed-looking claim, matching how the backend treats the same field.
  assert.deepEqual(decodeSessionClaimsProfile(session), { email: 'wrapped@example.com', name: '' });
});

test('decodeSessionClaimsProfile returns null rather than throwing, for every input that is not a usable JWT', () => {
  for (const bad of [
    '', 'not-a-jwt', 'a.b', 'a.b.c.d',
    '{"accessToken":123}', '{not json',
    fakeJWT({}), // valid JWT, no profile claim at all
    fakeJWT({ 'https://api.openai.com/profile': {} }), // profile present, no email
    fakeJWT({ 'https://api.openai.com/profile': { email: 123 } }), // wrong type
    'header.' + Buffer.from('not json').toString('base64url') + '.sig',
  ]) {
    assert.equal(decodeSessionClaimsProfile(bad), null, `expected null for ${JSON.stringify(bad).slice(0, 60)}`);
  }
});

test('decodeSessionClaimsProfile handles base64url segments needing every padding length', () => {
  // atob rejects unpadded base64 — the payload segment's length mod 4 can be
  // 0, 2 or 3 (JWTs never produce 1), and the padding math must cover each.
  for (const email of ['a@b.co', 'ab@b.co', 'abc@b.co', 'abcd@b.co']) {
    const jwt = fakeJWT({ 'https://api.openai.com/profile': { email } });
    assert.equal(decodeSessionClaimsProfile(jwt)?.email, email, `padding case for ${email}`);
  }
});
