import type { Config } from '../api/types';

/**
 * includeSiteScript adds the site script the config names (e.g. the panda
 * menu) to the document head, once. Nothing happens without one.
 */
export function includeSiteScript(config: Pick<Config, 'uiScript' | 'uiScriptIntegrity'>): void {
  const src = config.uiScript;
  if (!src) return;
  if ([...document.scripts].some(s => s.getAttribute('src') === src)) return;

  const script = document.createElement('script');
  script.src = src;
  script.setAttribute('integrity', config.uiScriptIntegrity ?? '');
  script.setAttribute('crossorigin', 'anonymous');
  document.head.appendChild(script);
}
