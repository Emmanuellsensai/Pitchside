/**
 * Touchline on-chain handshake (devnet).
 *
 * Runs the TxLINE `subscribe` instruction, then the off-chain activation flow,
 * and prints a long-lived JWT plus the API token. Run this once, copy the two
 * strings into the proxy environment, done.
 *
 * Everything here is verified against CLAUDE.md and idl/txoracle.json. Do not
 * change addresses, seeds, or account names without updating that brief.
 */

import * as fs from "fs";
import * as path from "path";
import * as anchor from "@coral-xyz/anchor";
import { Program } from "@coral-xyz/anchor";
import {
  Connection,
  Keypair,
  PublicKey,
  SystemProgram,
  Transaction,
} from "@solana/web3.js";
import {
  TOKEN_2022_PROGRAM_ID,
  ASSOCIATED_TOKEN_PROGRAM_ID,
  getAssociatedTokenAddressSync,
  getAccount,
  createAssociatedTokenAccountInstruction,
} from "@solana/spl-token";
import nacl from "tweetnacl";

// Verified devnet constants (see CLAUDE.md).
const RPC_URL = "https://api.devnet.solana.com";
const PROGRAM_ID = new PublicKey("6pW64gN1s2uqjHkn1unFeEjAwJkPGHoppGvS715wyP2J");
const TXL_MINT = new PublicKey("4Zao8ocPhmMgq7PdsYWyxvqySMGx7xb9cMftPMkEokRG");
const API_BASE = "https://txline-dev.txodds.com";
const GUEST_FALLBACK_BASE = "https://txline.txodds.com";

// Service level. The brief lists 12 (real-time) with 1 as the fallback, but the
// on-chain pricing_matrix on this devnet deployment only defines row_id 1, which
// is free (price 0) and real-time (sampling_interval 0). 12 is rejected with
// 6059 (InvalidServiceLevelId), so we use the only valid level, 1. Weeks must be
// a multiple of 4. Empty leagues means the standard bundle.
const SERVICE_LEVEL_ID = 1;
const DURATION_WEEKS = 4;
const SELECTED_LEAGUES: string[] = [];

// Anchor program error codes we handle explicitly.
const ERR_ACTIVE_SUBSCRIPTION = 6016;
const ERR_INVALID_WEEKS = 6041;

const WALLET_PATH = path.join(__dirname, "dev-wallet.json");
const IDL_PATH = path.join(__dirname, "idl", "txoracle.json");

function loadWallet(): Keypair {
  const raw = JSON.parse(fs.readFileSync(WALLET_PATH, "utf8"));
  return Keypair.fromSecretKey(Uint8Array.from(raw));
}

function loadIdl(): anchor.Idl {
  return JSON.parse(fs.readFileSync(IDL_PATH, "utf8"));
}

/**
 * Pull a numeric Anchor error code out of whatever the rpc call threw. The new
 * anchor client exposes it at err.error.errorCode.number; older shapes keep it
 * on err.code. Fall back to scanning the message for a "custom program error".
 */
function anchorErrorCode(err: any): number | null {
  const fromError = err?.error?.errorCode?.number;
  if (typeof fromError === "number") return fromError;
  if (typeof err?.code === "number") return err.code;
  const msg: string = err?.message ?? "";
  const hex = msg.match(/custom program error: 0x([0-9a-fA-F]+)/);
  if (hex) return parseInt(hex[1], 16);
  return null;
}

async function postJson(url: string, body: unknown, bearer?: string) {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (bearer) headers["Authorization"] = `Bearer ${bearer}`;
  const res = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
  });
  const text = await res.text();
  let parsed: any = undefined;
  try {
    parsed = text ? JSON.parse(text) : undefined;
  } catch {
    parsed = text;
  }
  return { status: res.status, ok: res.ok, body: parsed, raw: text };
}

/**
 * Ensure the user Token-2022 ATA for TxL exists. The subscribe instruction
 * expects it present, so we create it first if the account is missing.
 */
async function ensureUserAta(
  connection: Connection,
  wallet: Keypair,
  userTokenAccount: PublicKey
): Promise<void> {
  try {
    await getAccount(connection, userTokenAccount, "confirmed", TOKEN_2022_PROGRAM_ID);
    console.log("User TxL ATA already exists:", userTokenAccount.toBase58());
    return;
  } catch {
    // Not found, create it below.
  }
  console.log("Creating user TxL ATA (Token-2022):", userTokenAccount.toBase58());
  const ix = createAssociatedTokenAccountInstruction(
    wallet.publicKey, // payer
    userTokenAccount, // ata
    wallet.publicKey, // owner
    TXL_MINT, // mint
    TOKEN_2022_PROGRAM_ID,
    ASSOCIATED_TOKEN_PROGRAM_ID
  );
  const tx = new Transaction().add(ix);
  const sig = await connection.sendTransaction(tx, [wallet]);
  await connection.confirmTransaction(sig, "confirmed");
  console.log("ATA created in tx:", sig);
}

/**
 * Find the most recent successful transaction the wallet signed that touched
 * the program. Used to reuse the existing subscription when subscribe returns
 * 6016 (ActiveSubscription) and we therefore have no fresh signature.
 */
async function findExistingSubscribeSig(
  connection: Connection,
  wallet: PublicKey
): Promise<string | null> {
  const sigs = await connection.getSignaturesForAddress(wallet, { limit: 100 }, "confirmed");
  for (const s of sigs) {
    if (s.err) continue;
    const tx = await connection.getTransaction(s.signature, {
      maxSupportedTransactionVersion: 0,
      commitment: "confirmed",
    });
    const keys = tx?.transaction.message.staticAccountKeys?.map((k) => k.toBase58()) ?? [];
    if (keys.includes(PROGRAM_ID.toBase58())) {
      return s.signature;
    }
  }
  return null;
}

async function main() {
  const wallet = loadWallet();
  const idl = loadIdl();
  const connection = new Connection(RPC_URL, "confirmed");

  console.log("Wallet:", wallet.publicKey.toBase58());
  const balance = await connection.getBalance(wallet.publicKey);
  console.log("Balance:", balance / 1e9, "SOL");
  if (balance === 0) {
    throw new Error("Wallet has 0 SOL. Airdrop devnet SOL before running.");
  }

  const provider = new anchor.AnchorProvider(
    connection,
    new anchor.Wallet(wallet),
    { commitment: "confirmed" }
  );
  anchor.setProvider(provider);

  // Anchor 0.30+: program id is read from idl.address.
  const program = new Program(idl, provider);

  // PDAs and ATAs, derived exactly as the brief specifies.
  const [tokenTreasuryPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("token_treasury_v2")],
    PROGRAM_ID
  );
  const tokenTreasuryVault = getAssociatedTokenAddressSync(
    TXL_MINT,
    tokenTreasuryPda,
    true,
    TOKEN_2022_PROGRAM_ID
  );
  const [pricingMatrixPda] = PublicKey.findProgramAddressSync(
    [Buffer.from("pricing_matrix")],
    PROGRAM_ID
  );
  const userTokenAccount = getAssociatedTokenAddressSync(
    TXL_MINT,
    wallet.publicKey,
    false,
    TOKEN_2022_PROGRAM_ID
  );

  console.log("tokenTreasuryPda:  ", tokenTreasuryPda.toBase58());
  console.log("tokenTreasuryVault:", tokenTreasuryVault.toBase58());
  console.log("pricingMatrixPda:  ", pricingMatrixPda.toBase58());
  console.log("userTokenAccount:  ", userTokenAccount.toBase58());

  // Create the user ATA first if it does not exist.
  await ensureUserAta(connection, wallet, userTokenAccount);

  // 1. subscribe. On 6016 reuse the existing subscription.
  let txSig: string;
  try {
    console.log(`\nSending subscribe(${SERVICE_LEVEL_ID}, ${DURATION_WEEKS})...`);
    txSig = await program.methods
      .subscribe(SERVICE_LEVEL_ID, DURATION_WEEKS)
      .accounts({
        user: wallet.publicKey,
        pricingMatrix: pricingMatrixPda,
        tokenMint: TXL_MINT,
        userTokenAccount: userTokenAccount,
        tokenTreasuryVault: tokenTreasuryVault,
        tokenTreasuryPda: tokenTreasuryPda,
        tokenProgram: TOKEN_2022_PROGRAM_ID,
        systemProgram: SystemProgram.programId,
        associatedTokenProgram: ASSOCIATED_TOKEN_PROGRAM_ID,
      })
      .rpc();
    console.log("subscribe confirmed in tx:", txSig);
  } catch (err: any) {
    const code = anchorErrorCode(err);
    if (code === ERR_ACTIVE_SUBSCRIPTION) {
      console.log("Program returned 6016 (ActiveSubscription). Reusing existing subscription.");
      const existing = await findExistingSubscribeSig(connection, wallet.publicKey);
      if (!existing) {
        throw new Error(
          "Active subscription exists but no prior program transaction was found " +
            "for this wallet to reuse for activation."
        );
      }
      txSig = existing;
      console.log("Reusing prior subscribe tx:", txSig);
    } else if (code === ERR_INVALID_WEEKS) {
      throw new Error(`Program returned 6041 (InvalidWeeks): weeks (${DURATION_WEEKS}) must be a multiple of 4.`);
    } else {
      console.error("subscribe failed:", err?.message ?? err);
      throw err;
    }
  }

  // 2. Activation. Get a guest JWT, sign the message, activate the API token.
  console.log("\nRequesting guest JWT...");
  let guest = await postJson(`${API_BASE}/auth/guest/start`, {});
  if (guest.status === 404) {
    console.log("Devnet guest/start 404, falling back to public host for the JWT only...");
    guest = await postJson(`${GUEST_FALLBACK_BASE}/auth/guest/start`, {});
  }
  if (!guest.ok) {
    throw new Error(`guest/start failed: ${guest.status} ${guest.raw}`);
  }
  const jwt: string = guest.body?.token ?? guest.body;
  if (!jwt || typeof jwt !== "string") {
    throw new Error(`guest/start did not return a token: ${guest.raw}`);
  }
  console.log("Got guest JWT.");

  // message = `${txSig}:${leagues.join(",")}:${jwt}` signed with the wallet key.
  const message = `${txSig}:${SELECTED_LEAGUES.join(",")}:${jwt}`;
  const signatureBytes = nacl.sign.detached(
    new TextEncoder().encode(message),
    wallet.secretKey
  );
  const walletSignature = Buffer.from(signatureBytes).toString("base64");

  console.log("Activating API token...");
  const activation = await postJson(
    `${API_BASE}/api/token/activate`,
    { txSig, walletSignature, leagues: SELECTED_LEAGUES },
    jwt
  );
  if (!activation.ok) {
    throw new Error(`token/activate failed: ${activation.status} ${activation.raw}`);
  }
  const apiToken: string =
    activation.body?.data?.token ?? activation.body?.data ?? activation.body;
  if (!apiToken || typeof apiToken !== "string") {
    throw new Error(`token/activate did not return an apiToken: ${activation.raw}`);
  }

  // Final output: the two strings the proxy needs.
  console.log("\n========================================================");
  console.log("SUCCESS. Copy these into the proxy environment:");
  console.log("========================================================");
  console.log("TXLINE_JWT=" + jwt);
  console.log("TXLINE_API_TOKEN=" + apiToken);
  console.log("========================================================");
}

main().catch((err) => {
  console.error("\nHandshake failed:", err?.message ?? err);
  process.exit(1);
});
