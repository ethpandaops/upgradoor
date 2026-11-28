import { describe, expect, it } from 'vitest';

import golden from '../api/wire.golden.json';
import { AREAS, FIELDS, SECTIONS, sectionFields } from './settingsLayout';

describe('the settings layout', () => {
  // The settings as the controller serves them (written by TestWireFiles): a
  // setting the controller gains and the layout does not place fails here.
  const served = Object.keys((golden.rest.settings as { settings: object }).settings).sort();

  it('places every setting the controller serves exactly once', () => {
    const placed = SECTIONS.flatMap(sectionFields).map(f => f.key);
    expect([...placed].sort()).toEqual(served);
    expect(new Set(placed).size).toBe(placed.length);
    expect(FIELDS.map(f => f.key)).toEqual(placed);
  });

  it('names each section once, and has no section without a setting', () => {
    const ids = AREAS.flatMap(a => a.sections.map(s => s.id));
    expect(new Set(ids).size).toBe(ids.length);
    for (const section of SECTIONS) expect(sectionFields(section).length, section.id).toBeGreaterThan(0);
  });

  it('keeps every parent in the section of its child, as a chain without loops', () => {
    for (const section of SECTIONS) {
      const keys = new Map(sectionFields(section).map(f => [f.key, f]));
      expect(section.master?.parent, `${section.id} master`).toBeUndefined();

      for (const field of section.fields) {
        if (field.parent === undefined) continue;
        expect(keys.has(field.parent), `${field.key} -> ${field.parent}`).toBe(true);
        // Only a policy per name has entries that inherit.
        expect(['kinds', 'teams'], field.key).toContain(field.kind);

        const seen = new Set([field.key]);
        for (let p = keys.get(field.parent); p; p = p.parent === undefined ? undefined : keys.get(p.parent)) {
          expect(seen.has(p.key), `${field.key} loops`).toBe(false);
          seen.add(p.key);
        }
      }
    }
  });

  it('nests the image policies three levels: images, kind, team', () => {
    const images = SECTIONS.find(s => s.id === 'images');
    expect(images?.master?.key).toBe('policyImage');
    const parent = (key: string) => images?.fields.find(f => f.key === key)?.parent;
    expect(parent('policyKinds')).toBe('policyImage');
    expect(parent('policyClients')).toBe('policyKinds');
  });

  it('puts the global pause first, above the rollout kinds with their policy in the header', () => {
    const masters = SECTIONS.flatMap(s => (s.master ? [s.master] : []));
    expect(masters.map(m => m.key)).toEqual(['paused', 'policyImage', 'policyPackages', 'policyReboot']);
    expect(SECTIONS[0]?.master?.byAction).toBe(true);
    for (const m of masters.slice(1)) expect(m.kind).toBe('policy');
  });
});
