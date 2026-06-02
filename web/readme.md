# TeleCloud Frontend Assets

This repository contains the source code for the [TeleCloud](https://github.com/dabeecao/telecloud-go) web interface.

## 📁 Repository Structure
- `templates/`: Go HTML templates.
- `static/`: Frontend assets (CSS, JS, Fonts).
- `static/locales/`: JSON translation files.
- `tailwind.config.js`: Tailwind CSS configuration.
- `package.json`: Dependencies and build scripts.
- `build.js`: Unified build engine (Tailwind, esbuild, sync/minify locales, download static libs).
- `sync_locales.js`: Utility script to sync missing translation keys from `en.json` into all other locale files (run via `bun run sync-locales`).

## 🌍 Contributing Translations (Localization)

### English
If you would like to contribute a new language or improve an existing translation, please follow these steps:

1.  **Locate translation files**: Language files are located in the `static/locales/` directory in JSON format (e.g., `en.json`, `vi.json`).
2.  **Add a new language**:
    *   Create a new JSON file with the ISO language code (e.g., `fr.json` for French).
    *   Copy the content from `en.json` and translate the values into your language.
    *   Open `static/js/common.js` and make the following changes:
        1. Add the new language to the `availableLangs` array:
            ```javascript
            { code: 'fr', name: 'Français', flag: '🇫🇷' }
            ```
        2. Add the language code to the `supported` array (used for browser language auto-detection):
            ```javascript
            const supported = ['vi', 'en', ..., 'fr'];
            ```
        3. Add a locale mapping to the `localeMap` object (used for date formatting):
            ```javascript
            'fr': 'fr-FR'
            ```
3.  **Submit a Pull Request**: Once finished, submit a Pull Request (PR) to this repository.


## 🛠️ Build Process
The main TeleCloud repository integrates this as a git submodule. During the build process (Docker or GitHub Actions), the following steps are performed:
1. Fetch this submodule into the `web/` directory.
2. Run `build-frontend.sh` (Linux/macOS) or `build-frontend.ps1` (Windows) to generate minified assets (`*.min.js`, `*.min.css`, `*.min.json`).
3. Compile the Go binary with embedded assets.

### What the build script does
1. Cleans up old minified files (`*.min.css`, `*.min.js`, `*.min.json`).
2. Optionally pulls latest changes from `origin/main` (pass `1` as first argument to enable).
3. Verifies bun is installed — installs it automatically if not.
4. Runs `bun install` to install dependencies.
5. Executes `build.js` which handles (all in parallel):
   - Building Tailwind CSS via `@tailwindcss/cli`.
   - Bundling and minifying JS and CSS files via `esbuild`.
   - Minifying theme CSS files.
   - Syncing and minifying JSON locale files.
   - Downloading static libraries from CDN:
     - `pdf.min.js` and `pdf.worker.min.js` (PDF.js v3.11.174 from cdnjs)

### Local Development
To manually build the frontend assets:
1. Ensure you have **[bun](https://bun.com/)** installed.
2. Run the build command:
   ```bash
   cd web
   bun install
   bun run build.js
   ```
   *Alternatively, you can use the wrapper scripts:*
   ```bash
   # Linux/macOS
   ./build-frontend.sh
   # Windows (PowerShell)
   .\build-frontend.ps1
   # Windows (CMD)
   build-frontend.bat
   ```

## ⚠️ Note
Minified files are ignored by git to keep the repository clean. They are generated only during the build process.

---
Developed by [@dabeecao](https://github.com/dabeecao)
