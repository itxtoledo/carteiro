// Helpers to hide sensitive data in the UI ("screenshot mode"): e-mail
// addresses are masked the same way as the logs (first character + *** per
// label, public suffix kept) and subjects are replaced by a token.
export const redactedToken = "••••••••";

export function maskEmail(address: string): string {
  if (!address || !address.includes("@")) return address;
  const at = address.lastIndexOf("@");
  const local = address.slice(0, at);
  const domain = address.slice(at + 1);
  const maskedLocal = (local.charAt(0) || "") + "***";
  const parts = domain.split(".").filter(Boolean);
  if (parts.length === 0) return maskedLocal + "@***";
  const tld = parts.pop()!;
  const reg = parts.pop();
  if (!reg) return maskedLocal + "@" + domain.charAt(0) + "***";
  return maskedLocal + "@" + reg.charAt(0) + "***." + tld;
}

export function maskList(addrs: string[]): string[] {
  return addrs.map(maskEmail);
}

const EMAIL_RE = /[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;

/** Masks every e-mail address found inside a free-text value. */
export function maskTextEmails(text: string): string {
  if (!text) return text;
  return text.replace(EMAIL_RE, (m) => maskEmail(m));
}
