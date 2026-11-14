import { list, type PublicImage } from '../api/types';
import { shortRevision } from '../lib/format';
import { Digest } from './Digest';
import { Badge, cx } from './primitives';

/**
 * ImageStrip summarises each EL / CL image across the fleet:
 * "sigp/lighthouse:unstable abc123… on 12 of 14".
 */
export function ImageStrip({ images }: { images: readonly PublicImage[] }) {
  if (images.length === 0) return null;

  return (
    <ul aria-label="Images" className="flex flex-wrap gap-1.5">
      {images.map(img => {
        const on = list(img.hostsOn);
        const behind = list(img.hostsBehind);
        const total = on.length + behind.length;

        return (
          <li
            key={`${img.kind}/${img.image}`}
            className={cx(
              'inline-flex items-center gap-1.5 rounded-sm border bg-surface px-1.5 py-0.5 text-xs/5',
              behind.length > 0 ? 'border-warning' : 'border-border'
            )}
          >
            <Badge>{img.kind.toUpperCase()}</Badge>
            <span className="font-mono">{img.image}</span>
            <Digest value={img.digest} />
            {img.revision && <span className="font-mono text-faint">{shortRevision(img.revision)}</span>}
            <span
              className={behind.length > 0 ? 'font-medium text-warning' : 'text-muted'}
              title={behind.length > 0 ? `behind: ${behind.join(', ')}` : `on it: ${on.join(', ')}`}
            >
              on {on.length} of {total}
            </span>
          </li>
        );
      })}
    </ul>
  );
}
