---
slug: top-up
title: Top up
group: Account & keys
order: 12
intro: HypiToken bills per token in real time. New accounts get trial credit; add balance under Billing before sustained use.
---

## Steps

### 1. Open Billing

Log in to the console and click **Billing** in the left menu, or go to [api.novadiffusion.com/app/billing](https://api.novadiffusion.com/app/billing).

### 2. Choose an amount

Pick a preset or enter a custom amount — **from $1** to get started.

### 3. Pay

**Alipay** is preselected; you can switch to **card**. Follow the prompts to pay and your balance updates automatically — no need to contact support.

> **What happens when the balance runs out?** API requests return `402 Payment Required` and the client shows "request failed." Enable the low-balance alert in the console to avoid interruptions.

## How billing works

Per-token, in real time: after each request, the cost is `input tokens × input price + output tokens × output price`, deducted from your balance. Prices vary by model — see the [Pricing](/docs/pricing) page in the console for the rates in effect.

> Once topped up, head to [connect a client](/docs/codex-cli) and start using it.
