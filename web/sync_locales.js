const fs = require('fs');
const path = require('path');

const LOCALES_DIR = path.join(__dirname, 'static', 'locales');
const SOURCE_FILE = 'en.json';

function sync() {
    const sourcePath = path.join(LOCALES_DIR, SOURCE_FILE);
    
    if (!fs.existsSync(sourcePath)) {
        console.error(`Source file not found: ${sourcePath}`);
        process.exit(1);
    }

    let sourceData = JSON.parse(fs.readFileSync(sourcePath, 'utf8'));

    // Sort en.json itself first
    const sortedSource = {};
    Object.keys(sourceData).sort().forEach(k => { sortedSource[k] = sourceData[k]; });
    if (JSON.stringify(sourceData) !== JSON.stringify(sortedSource)) {
        fs.writeFileSync(sourcePath, JSON.stringify(sortedSource, null, 4), 'utf8');
        sourceData = sortedSource;
        console.log(`Syncing ${SOURCE_FILE}: Sorted keys A-Z`);
    }

    const files = fs.readdirSync(LOCALES_DIR);

    files.forEach(file => {
        // Only process .json files, skip source file and minified files
        if (file.endsWith('.json') && !file.endsWith('.min.json') && file !== SOURCE_FILE) {
            const targetPath = path.join(LOCALES_DIR, file);
            
            let targetData = {};
            try {
                targetData = JSON.parse(fs.readFileSync(targetPath, 'utf8'));
            } catch (e) {
                console.warn(`Could not read ${file}, starting fresh.`);
            }

            let addedKeys = [];
            let removedKeys = [];

            const syncObjects = (src, tgt, prefix = '') => {
                const res = {};
                
                // Add/Update keys from source
                Object.keys(src).forEach(k => {
                    const fullKey = prefix ? `${prefix}.${k}` : k;
                    if (tgt[k] === undefined) {
                        res[k] = src[k];
                        addedKeys.push(fullKey);
                    } else if (typeof src[k] === 'object' && src[k] !== null && !Array.isArray(src[k])) {
                        res[k] = syncObjects(src[k], tgt[k] || {}, fullKey);
                    } else {
                        res[k] = tgt[k];
                    }
                });

                // Check for keys to remove
                Object.keys(tgt).forEach(k => {
                    const fullKey = prefix ? `${prefix}.${k}` : k;
                    if (src[k] === undefined) {
                        removedKeys.push(fullKey);
                    }
                });

                return res;
            };

            const updatedData = syncObjects(sourceData, targetData);
            
            if (addedKeys.length > 0 || removedKeys.length > 0) {
                console.log(`Syncing ${file}:`);
                if (addedKeys.length > 0) console.log(`  - Added ${addedKeys.length} keys: [${addedKeys.join(', ')}]`);
                if (removedKeys.length > 0) console.log(`  - Removed ${removedKeys.length} keys: [${removedKeys.join(', ')}]`);
                
                // Sort keys alphabetically
                const sortedData = {};
                Object.keys(updatedData).sort().forEach(key => {
                    sortedData[key] = updatedData[key];
                });

                fs.writeFileSync(targetPath, JSON.stringify(sortedData, null, 4), 'utf8');
            } else {
                console.log(`Syncing ${file}: Already up to date.`);
            }
        }
    });
}

sync();
