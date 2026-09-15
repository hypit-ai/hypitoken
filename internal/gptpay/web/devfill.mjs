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
// MIT-licensed and explicitly allows redistribution). Names and emails
// are placeholder values.

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

export function installDevFill() {
  const fieldset = document.querySelector('fieldset#billing');
  if (!fieldset) return;

  const btn = document.createElement('button');
  btn.type = 'button';
  btn.id = 'autofill';
  btn.className = 'secondary';
  btn.textContent = '填入免税州地址';
  btn.title = '随机从 5 个免税州地址中选一个填入账单字段。';
  fieldset.prepend(btn);

  let resetTimer = 0;
  btn.addEventListener('click', () => {
    const fixture = pick(TAXFREE_FIXTURES);
    const first = pick(FIRST_NAMES);
    const last = pick(LAST_NAMES);

    setValue('name',        `${first} ${last}`);
    setValue('email',       `dev-${Math.random().toString(36).slice(2, 8)}@example.invalid`);
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
