// Image references, as far as the page looks at them: whether a reference
// names a tag. The agent decides; this is the hint next to the input.

/** validateImageRef returns what is wrong with a reference for an override, or null: it must name a tag. */
export function validateImageRef(ref: string): string | null {
  if (ref === '') return 'is empty';
  if (/\s/.test(ref)) return 'must not contain blanks';
  if (ref.includes('@')) return 'must name a tag (repository:tag), not a digest';

  const tagAt = ref.indexOf(':', ref.lastIndexOf('/') + 1);
  if (tagAt < 0 || tagAt === ref.length - 1) return 'must name a tag (repository:tag)';
  if (repository(ref) === '' || repository(ref).endsWith('/')) return 'has no repository';

  return null;
}

/** repository is a reference without its tag. */
function repository(ref: string): string {
  const tagAt = ref.indexOf(':', ref.lastIndexOf('/') + 1);
  return tagAt < 0 ? ref : ref.slice(0, tagAt);
}
