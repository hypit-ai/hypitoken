// Local format checks only. Never verifies account access or card ownership.
export const PLANS = ['chatgptplusplan', 'chatgptprolite', 'chatgptpro', 'chatgptgoplan'];
export const CURRENCIES = 'USD AUD CAD GBP EUR CLP JPY INR IDR PKR THB MYR TWD VND PHP NGN ZAR KZT TZS EGP BRL SEK CZK PLN DKK NOK KRW COP MXN PEN HUF QAR RON ILS AED SGD NZD CHF SAR DZD LBP MAD YER'.split(' ');
export const COUNTRIES = 'AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW'.split(' ');

export function validSession(value) {
  if (typeof value !== 'string' || value.length > 100000) return false;
  let token = value.trim();
  if (token.startsWith('{')) {
    try { token = JSON.parse(token).accessToken; } catch { return false; }
  }
  return typeof token === 'string' && /^[A-Za-z0-9._~+-]{20,16384}$/.test(token);
}

export function validCard(value) {
  if (typeof value !== 'string' || !/^[\d -]+$/.test(value)) return false;
  const digits = value.replace(/[ -]/g, '');
  if (!/^\d{12,19}$/.test(digits) || /^0+$/.test(digits)) return false;
  let sum = 0;
  for (let i = digits.length - 1, double = false; i >= 0; i--, double = !double) {
    let n = Number(digits[i]);
    if (double && (n *= 2) > 9) n -= 9;
    sum += n;
  }
  return sum % 10 === 0;
}

export function validExpiry(value, now = new Date()) {
  if (typeof value !== 'string') return false;
  const match = /^(0[1-9]|1[0-2])\s*\/\s*(\d{2})$/.exec(value.trim());
  if (!match) return false;
  const month = Number(match[1]), year = 2000 + Number(match[2]);
  return year > now.getFullYear() || (year === now.getFullYear() && month >= now.getMonth() + 1);
}

export function validate(values, now = new Date()) {
  const errors = {};
  if (!PLANS.includes(values.plan)) errors.plan = '请选择列表中的订阅计划。';
  if (!COUNTRIES.includes(values.country)) errors.country = '请选择真实账单国家 / 地区。';
  if (!CURRENCIES.includes(values.currency)) errors.currency = '请选择列表中的货币。';
  if (!validSession(values.session)) errors.session = '请填写 accessToken，或含 accessToken 的完整 Session JSON。';
  if (!validCard(values['card-number'])) errors['card-number'] = '卡号格式或校验位不正确，请核对。';
  if (!validExpiry(values.expiry, now)) errors.expiry = '请填写未过期的有效期，格式 MM / YY。';
  if (typeof values.cvv !== 'string' || !/^\d{3,4}$/.test(values.cvv)) errors.cvv = 'CVV 应为 3 或 4 位数字。';
  return errors;
}
