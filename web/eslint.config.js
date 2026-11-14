import js from '@eslint/js';
import prettier from 'eslint-config-prettier';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['dist', 'node_modules', 'coverage'] },
  {
    files: ['src/**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.strictTypeChecked, prettier],
    languageOptions: {
      ecmaVersion: 2023,
      globals: { ...globals.browser },
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
      eqeqeq: ['error', 'always'],
      'no-console': ['error', { allow: ['warn', 'error'] }],
      '@typescript-eslint/consistent-type-imports': 'error',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
      // Numbers and booleans in template strings are intended throughout.
      '@typescript-eslint/restrict-template-expressions': ['error', { allowNumber: true, allowBoolean: true }],
      // Event handlers that start a promise handle its outcome themselves.
      '@typescript-eslint/no-misused-promises': ['error', { checksVoidReturn: { attributes: false } }],
    },
  },
  {
    files: ['*.{js,ts}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended, prettier],
    languageOptions: { globals: { ...globals.node } },
  }
);
