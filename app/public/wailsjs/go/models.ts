export namespace main {
	
	export class Settings {
	    url: string;
	    token: string;
	    large_context?: boolean;
	    workspaces?: string[];
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.token = source["token"];
	        this.large_context = source["large_context"];
	        this.workspaces = source["workspaces"];
	    }
	}

}

