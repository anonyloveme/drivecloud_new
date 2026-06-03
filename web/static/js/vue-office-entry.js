import { createApp, h, ref } from 'vue';
import VueOfficeDocx from '@vue-office/docx/lib/v3/vue-office-docx.mjs';
import VueOfficeExcel from '@vue-office/excel/lib/v3/vue-office-excel.mjs';
import VueOfficePptx from '@vue-office/pptx/lib/v3/vue-office-pptx.mjs';

import '@vue-office/docx/lib/v3/index.css';
import '@vue-office/excel/lib/v3/index.css';

const componentMap = {
    docx: VueOfficeDocx,
    xlsx: VueOfficeExcel,
    xls: VueOfficeExcel,
    pptx: VueOfficePptx,
};

let currentApp = null;

window.VueOffice = {
    render(container, type, arrayBuffer) {
        this.destroy();

        const ext = type.toLowerCase();
        const component = componentMap[ext];
        if (!component) {
            throw new Error('Unsupported file format: .' + ext + '. Only docx, xlsx, xls, pptx are supported.');
        }

        const src = ref(arrayBuffer);

        const app = createApp({
            render() {
                return h(component, { src: src.value, style: 'height: 100%;' });
            }
        });

        app.mount(container);
        currentApp = app;
    },

    destroy() {
        if (currentApp) {
            try {
                currentApp.unmount();
            } catch (e) {
                // ignore
            }
            currentApp = null;
        }
    }
};
