import { COUNTRIES, CURRENCIES, validate, validSession } from './validation.mjs';
import { installDevFill } from './devfill.mjs';

const $ = id => document.getElementById(id);
const form = $('recharge'), result = $('result');
const fields = ['plan', 'country', 'currency', 'proxy', 'session', 'card-number', 'expiry', 'cvv'];
const billingFields = ['name', 'email', 'line1', 'city', 'postal_code', 'state'];
let enabled = false, busy = false, flow = '', quoted = null, attempted = false, generation = 0, timer;
const controllers = new Set();
function displayNames(type) { try { return new Intl.DisplayNames(['zh-CN'], { type }); } catch { return { of: x => x }; } }
const regions = displayNames('region'), currencies = displayNames('currency');
for (const code of COUNTRIES) $('country').add(new Option(`${regions.of(code)} · ${code}`, code));
$('currency').replaceChildren(...CURRENCIES.map(code => new Option(`${code} · ${currencies.of(code)}`, code)));
$('currency').value = 'USD';
const values = () => Object.fromEntries(fields.map(id => [id, $(id).value]));
const billing = () => ({ ...Object.fromEntries(billingFields.map(id => [id, $(id).value.trim()])), country: $('country').value });
function message(text, error = false) { result.textContent = text; result.dataset.error = String(error); }
function controls() {
  $('check').disabled = !enabled || busy || attempted;
  $('pay').hidden = !quoted || attempted; $('pay').disabled = !enabled || busy || attempted;
  $('status').hidden = !flow; $('status').disabled = busy;
  $('reset').disabled = busy; $('recover').disabled = !enabled || busy;
  for (const id of [...fields, ...billingFields]) $(id).disabled = busy || (attempted && id !== 'session');
}
function errors(pay = false) {
  const e = validate(values());
  if ($('proxy').value.trim()) { try { const p = new URL($('proxy').value.trim()); if (!['socks5:', 'socks5h:'].includes(p.protocol) || !p.hostname || !p.port || p.pathname || p.search || p.hash) throw new Error(); } catch { e.proxy = '请填写完整 SOCKS5 地址和端口，或留空直连。'; } }
  if (!pay) for (const id of ['card-number', 'expiry', 'cvv']) delete e[id];
  for (const id of fields) { $(id).setAttribute('aria-invalid', String(!!e[id])); if ($(`${id}-error`)) $(`${id}-error`).textContent = e[id] || ''; }
  for (const id of billingFields) {
    const invalid = !$(id).checkValidity(); $(id).setAttribute('aria-invalid', String(invalid));
    if (invalid) e[id] = '请补全真实账单信息。';
  }
  const first = Object.keys(e)[0]; if (first) { $(first).focus(); message(e[first], true); return true; }
  return false;
}
async function api(path, data) {
  const controller = new AbortController(); controllers.add(controller);
  const timeout = setTimeout(() => controller.abort(), 190000);
  try {
    const response = await fetch(`/api/${path}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ...data, session: $('session').value, proxy: $('proxy').value.trim() }), cache: 'no-store', credentials: 'same-origin', signal: controller.signal });
    const body = await response.json(); if (!response.ok) throw new Error(body.error || '请求失败，请查询支付结果。'); return body;
  } catch (e) {
    if (e instanceof TypeError || e.name === 'AbortError') throw new Error('网络中断，付款结果可能未知。请查询结果，不要重新付款。');
    throw e;
  } finally { clearTimeout(timeout); controllers.delete(controller); }
}
function amount(q) {
  const fmt = new Intl.NumberFormat('zh-CN', { style: 'currency', currency: q.currency });
  const digits = fmt.resolvedOptions().maximumFractionDigits;
  return `${fmt.format(q.amount_minor / 10 ** digits)} ${q.currency}`;
}
function invalidate() {
  if (!attempted) { quoted = null; $('quote').hidden = true; }
  $('service-state').textContent = !enabled ? '服务未启用 · 不会扣款' : $('proxy').value.trim() ? '使用所填代理 · 确认后付款' : '服务器直连 · 确认后付款';
  $('plan-note').hidden = $('plan').value !== 'chatgptprolite'; controls();
}
for (const id of [...fields, ...billingFields]) $(id).addEventListener('input', () => {
  if (['plan', 'country', 'currency', 'proxy', 'session'].includes(id) && !attempted) { flow = ''; $('checkout-ref').hidden = true; }
  if (!['card-number', 'expiry', 'cvv'].includes(id)) invalidate();
});
// Digits are the source of truth; the display format is re-derived on every
// keystroke. That makes backspace behave naturally (deleting a digit also
// drops a separator that no longer has two digits to sit between) without
// tracking cursor position by hand.
$('card-number').addEventListener('input', e => {
  const digits = e.target.value.replace(/\D/g, '').slice(0, 19);
  e.target.value = digits.replace(/(\d{4})(?=\d)/g, '$1 ');
});
$('expiry').addEventListener('input', e => {
  const digits = e.target.value.replace(/\D/g, '').slice(0, 4);
  e.target.value = digits.length > 2 ? `${digits.slice(0, 2)}/${digits.slice(2)}` : digits;
});
form.addEventListener('submit', async event => {
  event.preventDefault(); if (busy || attempted) return;
  if (!enabled || errors()) return;
  busy = true; controls(); message($('proxy').value.trim() ? '正在通过所填代理创建结账并获取报价…' : '正在由服务器直连创建结账并获取报价…');
  try {
    if (!flow) {
      const created = await api('create', { selection: { plan: $('plan').value, country: $('country').value, currency: $('currency').value }, billing: billing() });
      flow = created.flow;
      $('checkout-ref').textContent = `结账编号：${created.checkout.checkout_session_id} · ${created.checkout.processor_entity}（中断后查询用）。官方结账地址：https://chatgpt.com/checkout/${created.checkout.processor_entity}/${created.checkout.checkout_session_id}`;
      $('checkout-ref').hidden = false;
      $('recover-id').value = created.checkout.checkout_session_id; $('recover-entity').value = created.checkout.processor_entity;
    }
    quoted = await api('quote', { flow, billing: billing() });
    $('quote').textContent = `${$('plan').selectedOptions[0].textContent} · ${quoted.account} · 含税应付 ${amount(quoted)}（最小单位 ${quoted.amount_minor}）。报价有效至 ${new Date(quoted.expires_at * 1000).toLocaleTimeString()}。`;
    $('quote').hidden = false; message('请核对账号、套餐和金额。尚未扣款。');
  } catch (e) { quoted = null; $('quote').hidden = true; message(e.message, true); }
  finally { busy = false; controls(); }
});
function renderStatus(data) {
  const messages = { paid: '支付成功，结账已完成。', processing: '支付正在处理中，请稍后查询。', pending: '尚未确认支付成功，请稍后查询，不要重复付款。', requires_action: '需要银行 / 3DS 验证，自动流程已暂停。请在官方结账页完成验证；如选择了代理，请确保浏览器也使用同一代理。', requires_payment_method: '支付方式未通过。请核对官方结账状态，不要重复提交本次付款。', expired: '结账已过期。', canceled: '支付已取消。' };
  message(messages[data.paid ? 'paid' : data.state] || '支付结果未知，请继续查询。', !data.paid && ['requires_payment_method', 'expired', 'canceled'].includes(data.state));
  return data.paid || ['requires_action', 'requires_payment_method', 'expired', 'canceled'].includes(data.state);
}
async function checkStatus(polls = 0, epoch = generation) {
  if (busy || !flow || epoch !== generation) return;
  busy = true; controls(); let terminal = true;
  try { terminal = renderStatus(await api('status', { flow })); } catch (e) { message(e.message, true); }
  finally { busy = false; controls(); }
  if (!terminal && polls > 0 && epoch === generation) timer = setTimeout(() => checkStatus(polls - 1, epoch), 3000);
}
$('pay').addEventListener('click', async () => {
  if (busy || attempted || !quoted || errors(true)) return;
  if (Date.now() >= quoted.expires_at * 1000) { invalidate(); message('报价已过期，请重新获取。', true); return; }
  if (!window.confirm(`确认支付 ${amount(quoted)}？\n套餐：${$('plan').selectedOptions[0].textContent}\n账号：${quoted.account}\n将提交真实付款。`)) return;
  attempted = true; busy = true; controls(); message('正在提交付款，请勿重复操作或刷新…');
  const [month, yy] = $('expiry').value.replace(/\s/g, '').split('/');
  const card = { number: $('card-number').value.replace(/[ -]/g, ''), month, year: `20${yy}`, cvc: $('cvv').value };
  let poll = false;
  try { poll = !renderStatus(await api('pay', { flow, confirm: true, amount_minor: quoted.amount_minor, currency: quoted.currency, card })); }
  catch (e) { message(`${e.message} 本次付款已锁定，请仅查询结果。`, true); }
  finally { for (const k of Object.keys(card)) card[k] = ''; for (const id of ['card-number', 'expiry', 'cvv']) $(id).value = ''; busy = false; controls(); }
  if (poll) timer = setTimeout(() => checkStatus(9), 3000);
});
$('status').addEventListener('click', () => { clearTimeout(timer); checkStatus(); });
$('recover').addEventListener('click', async () => {
  if (busy || !enabled) return;
  if (!validSession($('session').value)) { message('请在上方填写原 Session。', true); $('session').focus(); return; }
  busy = true; controls();
  try { renderStatus(await api('recover', { checkout: { checkout_session_id: $('recover-id').value.trim(), processor_entity: $('recover-entity').value.trim() } })); }
  catch (e) { message(e.message, true); }
  finally { busy = false; controls(); }
});
function clearSensitive() { for (const id of ['proxy', 'session', 'card-number', 'expiry', 'cvv', ...billingFields]) $(id).value = ''; }
form.addEventListener('reset', event => {
  if (busy || (attempted && !window.confirm('清空不会撤销付款。请先保存结账编号，避免重新付款。'))) { event.preventDefault(); return; }
  generation++; clearTimeout(timer); flow = ''; quoted = null; attempted = false; clearSensitive();
  $('quote').hidden = true; $('checkout-ref').hidden = true; message('');
  for (const id of fields) { $(id).removeAttribute('aria-invalid'); if ($(`${id}-error`)) $(`${id}-error`).textContent = ''; }
  queueMicrotask(invalidate);
});
window.addEventListener('pagehide', () => { generation++; clearTimeout(timer); for (const c of controllers) c.abort(); clearSensitive(); });
window.addEventListener('pageshow', event => { if (event.persisted) { clearSensitive(); message('页面已恢复。若曾付款，请使用原 Session 查询结果，不要重付。'); } });
try {
  const r = await fetch('/api/config', { cache: 'no-store', credentials: 'same-origin' }); const config = await r.json(); enabled = r.ok && config.enabled === true;
  $('service-state').textContent = enabled ? '服务器直连 · 确认后付款' : '服务未启用 · 不会扣款';
} catch { $('service-state').textContent = '服务不可用 · 不会扣款'; }
controls();
installDevFill();
