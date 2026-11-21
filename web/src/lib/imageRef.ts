// Image references, as far as the page looks at them: whether a reference
// names a tag, and whether its repository is one a host allows overrides
// from (overridable.images of a container). The agent decides; this is the
// hint next to the input.

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

/** qualified puts the registry in front of a repository that names none: docker.io, and library/ for a single name. */
function qualified(repo: string): string {
  const slash = repo.indexOf('/');
  if (slash < 0) return `docker.io/library/${repo}`;

  const first = repo.slice(0, slash);
  return first.includes('.') || first.includes(':') || first === 'localhost' ? repo : `docker.io/${repo}`;
}

/**
 * imageAllowed reports whether a tag reference is in one of the allowed
 * repositories. A pattern is a repository without a tag; "*" stands for any
 * run of characters without "/".
 */
export function imageAllowed(ref: string, patterns: readonly string[]): boolean {
  const repo = qualified(repository(ref));

  return patterns.some(pattern => {
    const source = qualified(pattern)
      .split('*')
      .map(part => part.replace(/[.+?^${}()|[\]\\]/g, '\\$&'))
      .join('[^/]*');
    return new RegExp(`^${source}$`).test(repo);
  });
}
