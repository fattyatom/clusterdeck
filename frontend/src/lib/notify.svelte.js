export const notice = $state({ open: false, title: "", text: "", tone: "" });

export function show(title, text, tone = "") {
  notice.title = title;
  notice.text = text || "(empty)";
  notice.tone = tone;
  notice.open = true;
}

export async function act(label, fn) {
  try {
    const out = await fn();
    show(label, out.result, "good");
    return true;
  } catch (e) {
    show(`${label} failed`, e.message, "bad");
    return false;
  }
}

export const confirmDialog = $state({ open: false, title: "", text: "", label: "Confirm", resolve: null });

// ask shows an in-page confirmation and resolves true only if the user confirms it.
export function ask(title, text, label = "Confirm") {
  confirmDialog.resolve?.(false);
  return new Promise((resolve) => {
    Object.assign(confirmDialog, { open: true, title, text, label, resolve });
  });
}

export function settle(ok) {
  const r = confirmDialog.resolve;
  confirmDialog.resolve = null;
  confirmDialog.open = false;
  r?.(ok);
}
