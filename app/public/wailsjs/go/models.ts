export namespace main {
	
	export class Settings {
	    url: string;
	    token: string;
	    workspaces?: string[];
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.token = source["token"];
	        this.workspaces = source["workspaces"];
	    }
	}

}

