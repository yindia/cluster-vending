/** Exhaust a list safely without silently presenting its first page as a total. */
export async function collectPages<T>(load: (token: string) => Promise<{ items: T[]; nextPageToken: string }>, signal?: AbortSignal): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let token = "";
  for (;;) {
    signal?.throwIfAborted();
    const page = await load(token);
    items.push(...page.items);
    if (!page.nextPageToken) return items;
    if (seen.has(page.nextPageToken)) throw new Error("The server returned a repeated pagination token; refresh the list.");
    seen.add(page.nextPageToken);
    token = page.nextPageToken;
  }
}
