[繁體中文](README.md) · engineers, see [`docs/README.md`](docs/README.md)

# crypto-exchange: a cryptocurrency exchange you can run on your own computer

This is the source code of an **exchange engine**: people place orders, orders are matched, money is booked, deposits come in and withdrawals go out, staff use a back office. Everything an exchange does behind the screen. All of it can run on your own computer, with pretend money on a pretend blockchain, so you can play the whole thing through from start to finish.

**It never connects to a real blockchain mainnet and there is no real money anywhere.** That is the project's iron rule, written on the first page of its plan. Everything you do here happens only on your computer.

This README is written for **people who have never done anything like this**: high-school students, anyone curious how an exchange works, anyone starting it up for the first time. You do not need to know how to program. What engineers want (architecture, commands, the document index) is in [`docs/README.md`](docs/README.md).

---

## 1. What is it? (two minutes)

A cryptocurrency exchange, taken apart, is these pieces:

| Piece | What it does | In this project |
|---|---|---|
| **Order book and matching** | Pairs "I want to buy 0.5 ETH at 2000" with "I want to sell 0.2 ETH at 2000" into a trade | Pure logic with no guesswork: the same orders always produce the same result |
| **Ledger** | Records how much everyone has, what is on hold, where fees went | **Double-entry bookkeeping**, as accountants do it: every amount has a source and a destination, and the total is always zero |
| **Deposits and withdrawals** | Receives money from a blockchain, pays money out to one | Talks to a pretend blockchain that runs on your computer (called **anvil**); it can also talk to Ethereum's Sepolia testnet, which is a separate, longer guide |
| **Market data** | Pushes the order book, trades and candles to the screen as they happen | WebSockets; you place an order and the page changes without a reload |
| **Trading front end** | The web page where you press the buttons | Switchable between Traditional Chinese and English |
| **Back office** | The web page the exchange's staff use: users, the ledger, withdrawal review, reconciliation | Switchable between the two languages too; signed in with a password plus a phone code |

It looks like this:

![the trading front end](docs/screenshots/web-trade.png)

![the back office dashboard](docs/screenshots/03-dashboard.png)

---

## 2. What you need before you start

**You need:**

- A computer running macOS, Windows 10/11 or Linux, with **8 GB of memory** or more and **5 GB of disk** free.
- **Docker**: it puts each of a dozen programs (the database, the pretend blockchain, the exchange itself) in its own "container" and starts them all with one command.
  - On macOS and Windows install [Docker Desktop](https://www.docker.com/products/docker-desktop/). On Windows, let it enable WSL 2 as it asks.
  - On Linux install Docker Engine plus the Compose plugin, following Docker's own instructions.
- **Git**: the tool that fetches the project onto your computer. On macOS type `xcode-select --install` in a terminal; on Ubuntu `sudo apt install -y git make`; on Windows do the same inside WSL.
- **make**: a small tool that remembers commands for you. The macOS step above installs it; so does the Ubuntu one.
- **A terminal**: Terminal on macOS, the WSL window on Windows, any terminal on Linux.
- **Internet** for the first run (about 2 GB to download); not afterwards.
- For step 9, **a phone with an authenticator app** (Google Authenticator, Microsoft Authenticator and 1Password all work).

**You do not need:** Node, Go, any programming language, any programming experience, any real money.

**How long it takes:** 20–30 minutes the first time, most of it waiting for downloads. Two minutes every time after that.

---

## 3. Starting it for the first time (type along)

Every step says what you will see. If what you see is different, look at section 4 first.

### Step 1: fetch the project

```sh
git clone https://github.com/arc119226/crypto-exchange.git
cd crypto-exchange
```

**You will see:** progress lines such as `Receiving objects`, and you end up inside the `crypto-exchange` folder. Every command from here on is typed in that folder.

### Step 2: generate keys that are yours alone

First make sure Docker is running (the Docker Desktop icon is green, or `docker ps` does not complain), then:

```sh
make gen-dev-secrets
```

**You will see:**

```
gen-dev-secrets: wrote .env
gen-dev-secrets: wrote secrets/jwt/ed25519.pem
gen-dev-secrets: wrote secrets/dev-mnemonic.txt (keep it: Phase 4 imports it into the keystore)
gen-dev-secrets: wrote secrets/keystore/hd-seed.json (hot wallet matches cast)
gen-dev-secrets: done — next: make up-single
```

What this did: an exchange needs passwords and keys (the database password, the key that signs login tokens, the wallet seed). This command **generates a random set that belongs to this computer only** and writes it to `.env` and the `secrets/` folder. They are yours: do not send them to anyone and do not share screenshots of them.

### Step 3: start the whole exchange

```sh
make up-single
```

**You will see:** the first run downloads images and compiles the program, about 5–15 minutes. The last lines show every container turning `Healthy` or `Started`, and the command returns to the prompt.

Check:

```sh
make ps
```

**You will see:** a dozen or so rows such as `postgres`, `redis`, `nats`, `anvil`, `exchange-all`, `web`, `grafana`, in state `running` or `healthy`. Three of them, `migrate`, `seed` and `contracts-deployer`, are one-off jobs and show `exited (0)`, which is right.

What this started:

- `postgres`: the database; every account is in here
- `anvil`: the pretend Ethereum blockchain; blocks are instant and the coins are worthless
- `exchange-all`: the exchange itself (matching, ledger, API, back office, all in one program)
- `web`: the web server for the trading front end
- `nats`, `redis`, `prometheus`, `grafana`, `jaeger`: messaging, caching, monitoring; nothing to do with them for now

### Step 4: open the trading front end

Open <http://localhost:8088> in a browser.

**You will see:** the market list with one row, `ETH-USDC`: buying and selling ETH (ether) for USDC (a stablecoin pegged 1:1 to the US dollar). The last-price column shows `—` because nobody has traded yet. The top right corner has **中文 / EN**; one click switches the language. The rest of this document describes the English interface.

### Step 5: register an account

Press **Register** at the top right, enter `alice@example.com` as the email and any password of at least 8 characters, and press **Create account**.

**You will see:** the market page again, now with a green dot at the top right (your private stream is connected), the first 8 characters of your account id, and a new **Wallet** link.

The email is never sent to or verified; it is just the account's name.

### Step 6: give yourself some pretend money

A new account has nothing in it. A real exchange would make you deposit from outside; this one has a development "faucet" that simply hands money over.

Press **Wallet** at the top. The first line says "Account ID" followed by a long string. Copy the whole string, go back to the terminal, and paste it in place of `<ACCOUNT_ID>`:

```sh
make faucet ACCOUNT=<ACCOUNT_ID> ASSET=USDC AMOUNT=10000
```

**You will see:** the terminal prints one ledger entry as JSON (`"kind": "adjustment"`, `"amount": "10000"`). **Without reloading**, go back to the browser: the wallet page's available USDC already reads `10000`. That is market data at work: the back end books the entry and pushes it to your screen at once.

In the ledger this money is booked as a debit of 10000 to an account called "external" and a credit of 10000 to your available balance. The two sides are equal. **Every amount in this project moves that way**, so there is never money on one side that has no match on the other.

### Step 7: place a bid

Go back to **Markets**, click the `ETH-USDC` row, and you are on the trading page. On the right is **Place order**:

1. Check that **Buy ETH** and **Limit** are selected
2. Enter `2000` as the price and `0.5` as the quantity
3. Press the green **Buy ETH**

**You will see:**

- A green line under the form: "open · filled 0"
- A new row on the bid side of the order book on the left, `2000 / 0.5`: your order is resting in the book, waiting for a seller
- A new row under **Open orders** below
- **Balances** on the right: USDC available `9000`, on hold `1000`

What "on hold" means: you promised to buy 0.5 at 2000, which is 1000 USDC in total. The money is still yours, but it is locked and cannot be used for anything else. If you could withdraw it while the order rests, you could not pay when the trade happens. The hold is released when the order fills or is cancelled.

### Step 8: be your own counterparty and watch it fill

A buyer alone does not make a trade; someone has to sell. Open a **new tab** (a new tab starts signed out), go to <http://localhost:8088> again, and register a second account, `bob@example.com`. Copy the account id from bob's wallet page and give him ETH this time:

```sh
make faucet ACCOUNT=<BOB_ACCOUNT_ID> ASSET=ETH AMOUNT=1
```

In bob's tab open `ETH-USDC`, choose **Sell ETH**, limit, price `2000`, quantity `0.2`, and press the red **Sell ETH**.

**Bob's screen shows:** "filled · filled 0.2". His balances: ETH `0.8`, USDC `399.2`.

**Switch back to alice's tab:**

- Her open order now reads "partially filled" with `0.2` filled
- **My fills** has a new row: 2000 × 0.2, role **maker**
- Balances: ETH `0.1998`, USDC available `9000`, on hold `600`
- The bid row in the order book now reads `2000 / 0.3` (0.3 still to buy)
- **Recent trades** has a new row

Why the numbers are not round: the exchange charges **fees**. Alice is the maker (she put her order in the book first and provided liquidity) and pays 0.1%: 0.2 × 0.1% = 0.0002 ETH, so she receives 0.1998. Bob is the taker (he came and took someone's resting order) and pays 0.2%: 400 × 0.2% = 0.8 USDC, so he receives 399.2. The trade always happens at the maker's price; 400 of alice's 1000 hold was spent and 600 stays on hold for the remaining 0.3.

To cancel the rest, press **Cancel** on alice's open order: the row leaves the book and the hold drops to zero.

### Step 9: into the back office

The back office is for the exchange's staff. Signing in takes a password **and** a phone code (TOTP), so first issue the code's secret to the default administrator:

```sh
make totp-enroll EMAIL=admin@example.com
```

**You will see:**

```
admin totp enroll: admin@example.com

  otpauth URL  otpauth://totp/...
  secret       XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
```

Open the authenticator app on your phone, choose manual entry, and type in the `secret` string (or turn the otpauth URL into a QR code and scan it). **This secret is shown once and never again.**

Then look up the administrator's password:

```sh
grep ADMIN_BOOTSTRAP_PASSWORD .env
```

Open <http://127.0.0.1:8082/admin/login> in the browser, enter `admin@example.com` and the password you just looked up, press **Continue**; on the next page enter the six digits from the app and press **Verify**. The top right corner switches languages here too.

**You will see the dashboard:**

- **Ledger: balanced**, 0 open breaks: everyone's money added up, debits equal credits
- **Chain reconciliation: BREAK** (in red). This is normal, and deliberate: the pretend blockchain simply put 100 ETH into the exchange's hot wallet at start-up, and no transaction in the ledger records it. Every few seconds reconciliation compares "what the ledger thinks there is" with "what is really on the chain", and reports that the chain has more. At a real exchange this means someone has to find out where the money came from; here it proves reconciliation is really looking.
- Withdrawals waiting for review 0, deposits confirming 0

A few more pages:

- **Users** → click `alice@example.com` → the link on the "Spot account" line → the **Ledger** page: every line of the faucet entry from step 6 and of the trade from step 8, whose money went from where to where, and which account the fee landed in.
- **Audit**: your sign-in just now and the two `make faucet` runs from steps 6 and 8 are all recorded. Everything done in the back office lands here and cannot be deleted.
- By the way, `make faucet` is the "Post an adjustment" form at the bottom of the **Ledger** page.

### Step 10: shut it down

```sh
make down
```

The data stays. The next `make up-single` is back within two minutes, with the accounts, balances and orders still there.

To start over (wiping every account and all data):

```sh
make reset
```

---

## 4. Something went wrong?

| You see | Cause | What to do |
|---|---|---|
| `Cannot connect to the Docker daemon` | Docker is not running | Open Docker Desktop, wait for the icon to turn green, try again |
| `port is already allocated` | Another program on your computer holds the port | This project uses 5432, 6379, 4222, 8545, 8080, 8081, 8082, 8088, 3000, 9090 and 16686. `lsof -i :8080` (with the number from the error) finds the culprit; close it |
| `make gen-dev-secrets` sits at a download | It pulls one image to compute the wallet address | Wait, or check the network; running it again is safe |
| `make up-single` fails half way or hangs | The first download was slow or dropped | Run `make up-single` again; what is already downloaded is not fetched twice |
| `make: command not found` | make is not installed | macOS `xcode-select --install`; Ubuntu `sudo apt install make`; on Windows do it inside WSL |
| `.env: No such file` | Step 2 was skipped | Run `make gen-dev-secrets` first |
| The dot at the top right of the front end is red or yellow | The exchange is not up yet, or just restarted | `make ps` to see whether `exchange-all` is `healthy`; `make logs SERVICE=exchange-all` to read what it says |
| The phone code is always wrong | The phone's clock is off (TOTP depends on time) | Set the phone's time to automatic; five wrong codes lock the account for 15 minutes, so wait |
| `make faucet` says `account does not exist` | The account id was pasted wrong or cut short | Copy the whole string from the "Account ID" line on the wallet page |
| You want to start over | — | `make reset`, then from step 3 |

Still stuck: paste the last 30 lines of the terminal and the output of `make ps` into a [GitHub issue](https://github.com/arc119226/crypto-exchange/issues). **Do not** paste the contents of `.env` or `secrets/`.

---

## 5. Words

| Word | Meaning |
|---|---|
| exchange | A place that pairs buyers with sellers and keeps both sides' books |
| order book | The list of all orders not yet traded, sorted by price. The gap between the highest bid and the lowest ask is the **spread** |
| limit order | "I will only trade at this price or better"; if nobody takes it, it rests in the book and waits |
| market order | "Trade now, at whatever price"; it eats through the book from the best price |
| maker / taker | The one who put an order in the book and waited / the one who came and took it. Trades happen at the maker's price; the taker usually pays the higher fee |
| fill | Part or all of an order being matched |
| available / hold | Money you can place orders or withdraw with / money you have promised to pay and that is locked |
| ledger | The book that records where every amount came from and went. Here it is **double-entry**: every entry has a debit and a credit of equal size, so at any moment "all debits minus all credits" is zero (the **trial balance**) |
| fee, bps | Basis point, one ten-thousandth. 10 bps = 0.1% |
| anvil | The pretend Ethereum blockchain running on your computer. Instant blocks, free coins, gone when you stop it |
| deposit / withdrawal | Moving coins from the blockchain into the exchange / out of the exchange to an address on the blockchain |
| withdrawal fee / deposit fee | The exchange may charge for either, but **both ship at zero**, so none of the numbers above are different because of them. A withdrawal fee is charged **on top**: the destination receives exactly what you typed and the account is debited the amount plus the fee. A deposit fee comes out of what arrived. An operator sets them on the Assets page, and a change only affects later requests |
| sweep | The exchange gathering coins scattered across deposit addresses into its own vault (the hot wallet) |
| reconciliation | Comparing the ledger's numbers with the real numbers on the blockchain; they may not differ by a cent |
| back office | The management web page the exchange's staff use |
| TOTP | The six digits an authenticator app changes every 30 seconds; the second lock on the back office |
| container | Docker's unit of one program packaged with what it needs; `make up-single` starts a dozen |

The full table is section 19 of the plan, [`docs/plan-v1.0.md`](docs/plan-v1.0.md) (in Traditional Chinese).

---

## 6. What to read next

- [`docs/README.md`](docs/README.md): the engineer's README. Architecture, what each folder is, the commands, the document index.
- [`docs/guides/sepolia.md`](docs/guides/sepolia.md): the same system on the **real** Ethereum testnet, Sepolia, once around. Also written for beginners, but it takes two or three hours.
- [`docs/plan-v1.0.md`](docs/plan-v1.0.md): the project's plan: why it is designed this way, which phases it was built in, what counts as done for each.
- [`docs/domain.md`](docs/domain.md): every kind of ledger entry and every order state, worked through line by line. "Where exactly is the fee booked?" is answered here.
- `web/trade/`: the trading front end's source (React). `internal/admin/`: the back office. Each side keeps its Chinese and English strings in one file; to change a word, change it there.

The design documents are written in Traditional Chinese; the code, its comments and the API documents are in English.

---

## 7. A word on safety

**This software has never had a security audit or a penetration test. It has no KYC, AML, sanctions-screening or Travel Rule capability. It is provided with no warranty of any kind. Do not operate it with real customer funds.** [`docs/limitations.md`](docs/limitations.md) is the whole list, written down rather than implied.

With that said, here is what protects you while you follow this guide:

- **Testnet only, and the binary enforces part of that.** What you start here talks to a pretend blockchain running on your own computer. Point it at a real network and it refuses to start against the mainnets it knows by chain id -- Ethereum, Base, Arbitrum One, BNB Smart Chain, Avalanche and a dozen more -- unless three separate conditions hold, and one of them cannot be satisfied in this build. That list can never be complete, though: anyone can stand up an EVM chain and choose an id, so pointing this at a mainnet the list does not name is **not** blocked by code. Past that line it is your discipline, not the software's.
- `.env` and `secrets/` hold this computer's keys. Do not share them, do not screenshot them, do not commit them (they are already in `.gitignore`).
- The back office is bound to `127.0.0.1` only, so only your own computer can reach it.
- The "money" here has no value. If you break something, `make reset`.
- **Running an exchange is a regulated activity nearly everywhere.** Licensing, sanctions screening and Travel Rule reporting belong to whoever operates a deployment, not to this project.
