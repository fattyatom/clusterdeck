export const notice = $state({ open: false, title: "", text: "" });

export function show(title, text) {
  notice.title = title;
  notice.text = text || "(empty)";
  notice.open = true;
}

export async function act(label, fn) {
  try {
    const out = await fn();
    show(label, out.result);
    return true;
  } catch (e) {
    show(`${label} failed`, e.message);
    return false;
  }
}
