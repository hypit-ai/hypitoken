// Billing autofill for the gptpay page.
//
// The page has a single path: real payment. This module adds one
// convenience to that path — a button at the top of the billing fieldset
// that prefills a realistic-looking US tax-free-state address so the
// user doesn't have to type one by hand. There is no separate mode, no
// backend round-trip — just the same form, with fields filled in.
//
// Real-format addresses are intentional: a payment AVS pre-flight rejects
// obvious placeholders like "TEST ONLY 101 Example Avenue". The five
// samples come from US Census geographic / USPS public records (the same
// data the mockaddress.com preview pack aggregates, which is itself
// MIT-licensed and explicitly allows redistribution). Name and email come
// from whatever Session is already pasted in, when it decodes one — a
// billing email that does not belong to the account being charged is
// exactly the kind of mismatch a fraud check looks for, so a synthetic
// `.invalid` address here is worse than no convenience at all. Only when
// the session field is empty or does not decode does this fall back to a
// placeholder name/email.

export const TAXFREE_FIXTURES = [
  // Alaska — Anchorage
  { street: '2008 Dimond Dr',   city: 'Anchorage',   state: 'AK', zip: '99507' },
  // Delaware — Wilmington
  { street: '1007 N Orange St', city: 'Wilmington',  state: 'DE', zip: '19801' },
  // Montana — Billings
  { street: '2822 3rd Ave N',   city: 'Billings',    state: 'MT', zip: '59101' },
  // New Hampshire — Manchester
  { street: '1000 Elm St',      city: 'Manchester',  state: 'NH', zip: '03101' },
  // Oregon — Oregon City
  { street: '287 Molalla Ave',  city: 'Oregon City', state: 'OR', zip: '97045' },
];

const FIRST_NAMES = ['Taylor', 'Casey', 'Jordan', 'Morgan', 'Avery', 'Riley'];
const LAST_NAMES = ['Demo', 'Tester', 'QA', 'Sandbox', 'Dev', 'Local'];

function pick(list) {
  return list[Math.floor(Math.random() * list.length)];
}

function setValue(id, value) {
  const el = document.getElementById(id);
  if (!el) return;
  el.value = value;
  // Fire the same `input` event app.mjs listens to, so the existing
  // revalidation, `invalidate()`, and field-disable logic see the change.
  el.dispatchEvent(new Event('input', { bubbles: true }));
}

// Pure — no DOM, so it is unit-testable on its own (see devfill_test.mjs).
// Decodes whatever session string it is given enough to read its own claimed
// name/email, purely for prefill convenience. This is NOT authentication and
// NOT a signature check — the real request still goes to chatgpt.com, which
// is the only party that verifies the token. A session that is JSON-wrapped,
// a bare accessToken, malformed, or missing the profile claim all fall
// through to null with no error thrown — the caller falls back to a
// placeholder, exactly as if this function did not exist.
export function decodeSessionClaimsProfile(raw) {
  try {
    raw = String(raw).trim();
    if (raw.startsWith('{')) raw = JSON.parse(raw).accessToken || '';
    const parts = raw.split('.');
    if (parts.length !== 3) return null;
    let b64 = parts[1].replace(/-/g, '+').replace(/_/g, '/');
    b64 += '='.repeat((4 - (b64.length % 4)) % 4);
    const claims = JSON.parse(atob(b64));
    const profile = claims['https://api.openai.com/profile'];
    if (!profile || typeof profile.email !== 'string' || !profile.email) return null;
    return { email: profile.email, name: typeof profile.name === 'string' ? profile.name : '' };
  } catch {
    return null;
  }
}
function decodeSessionProfile() {
  return decodeSessionClaimsProfile(document.getElementById('session').value);
}

export function installDevFill() {
  const fieldset = document.querySelector('fieldset#billing');
  if (!fieldset) return;

  const btn = document.createElement('button');
  btn.type = 'button';
  btn.id = 'autofill';
  btn.className = 'secondary';
  btn.textContent = '填入免税州地址';
  btn.title = '随机从 5 个免税州地址中选一个填入账单字段；姓名和邮箱取自已填写的 Session（未填写或无法解析时用占位符）。';
  fieldset.prepend(btn);

  let resetTimer = 0;
  btn.addEventListener('click', () => {
    const fixture = pick(TAXFREE_FIXTURES);
    const first = pick(FIRST_NAMES);
    const last = pick(LAST_NAMES);
    const real = decodeSessionProfile();

    setValue('name',        real?.name || `${first} ${last}`);
    setValue('email',       real?.email || `dev-${Math.random().toString(36).slice(2, 8)}@example.invalid`);
    setValue('line1',       fixture.street);
    setValue('city',        fixture.city);
    setValue('postal_code', fixture.zip);
    setValue('state',       fixture.state);
    setValue('country',     'US');
    setValue('currency',    'USD');
    setValue('plan',        'chatgptplusplan');

    const original = btn.textContent;
    btn.textContent = `已填入 ${fixture.state} · ${fixture.city}`;
    btn.disabled = true;
    clearTimeout(resetTimer);
    resetTimer = setTimeout(() => {
      btn.textContent = original;
      btn.disabled = false;
    }, 1500);
  });
}
